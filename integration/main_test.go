package integration_test

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/nu7hatch/gouuid"
	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/config"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
	"github.com/onsi/gomega/ghttp"

	"github.com/cloudfoundry-incubator/executor/api"
	"github.com/cloudfoundry-incubator/executor/client"
	GardenServer "github.com/cloudfoundry-incubator/garden/server"
	"github.com/cloudfoundry-incubator/garden/warden"
	wfakes "github.com/cloudfoundry-incubator/garden/warden/fakes"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/loggregatorlib/logmessage"

	"github.com/cloudfoundry-incubator/executor/integration/executor_runner"
)

func TestExecutorMain(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Integration Suite")
}

var executorPath string
var gardenServer *GardenServer.WardenServer
var runner *executor_runner.ExecutorRunner
var executorClient client.Client

var _ = BeforeSuite(func() {
	var err error

	executorPath, err = gexec.Build("github.com/cloudfoundry-incubator/executor", "-race")
	Ω(err).ShouldNot(HaveOccurred())
})

var _ = AfterSuite(func() {
	gexec.CleanupBuildArtifacts()
})

var _ = Describe("Main", func() {
	var (
		wardenAddr string

		fakeBackend *wfakes.FakeBackend

		logMessages      <-chan *logmessage.LogMessage
		logConfirmations <-chan struct{}
	)

	drainTimeout := 5 * time.Second
	aBit := drainTimeout / 5
	pruningInterval := time.Second

	eventuallyContainerShouldBeDestroyed := func(args ...interface{}) {
		Eventually(fakeBackend.DestroyCallCount, args...).Should(Equal(1))
		handle := fakeBackend.DestroyArgsForCall(0)
		Ω(handle).Should(Equal("some-handle"))
	}

	BeforeEach(func() {
		//gardenServer.Stop calls listener.Close()
		//apparently listener.Close() returns before the port is *actually* closed...?
		wardenPort := 9001 + CurrentGinkgoTestDescription().LineNumber
		executorAddr := fmt.Sprintf("127.0.0.1:%d", 1700+GinkgoParallelNode())

		executorClient = client.New(http.DefaultClient, "http://"+executorAddr)

		wardenAddr = fmt.Sprintf("127.0.0.1:%d", wardenPort)

		fakeBackend = new(wfakes.FakeBackend)
		fakeBackend.CapacityReturns(warden.Capacity{
			MemoryInBytes: 1024 * 1024 * 1024,
			DiskInBytes:   1024 * 1024 * 1024,
			MaxContainers: 1024,
		}, nil)

		gardenServer = GardenServer.New("tcp", wardenAddr, 0, fakeBackend)

		var loggregatorAddr string

		bidirectionalLogConfs := make(chan struct{})
		logConfirmations = bidirectionalLogConfs
		loggregatorAddr, logMessages = startFakeLoggregatorServer(bidirectionalLogConfs)

		runner = executor_runner.New(
			executorPath,
			executorAddr,
			"tcp",
			wardenAddr,
			loggregatorAddr,
			"the-loggregator-secret",
		)
	})

	AfterEach(func() {
		runner.KillWithFire()
		gardenServer.Stop()
	})

	allocNewContainer := func() (guid string, fakeContainer *wfakes.FakeContainer) {
		fakeContainer = new(wfakes.FakeContainer)
		fakeBackend.CreateReturns(fakeContainer, nil)
		fakeBackend.LookupReturns(fakeContainer, nil)
		fakeBackend.ContainersReturns([]warden.Container{fakeContainer}, nil)
		fakeContainer.HandleReturns("some-handle")

		id, err := uuid.NewV4()
		Ω(err).ShouldNot(HaveOccurred())

		guid = id.String()

		_, err = executorClient.AllocateContainer(guid, api.ContainerAllocationRequest{
			MemoryMB: 1024,
			DiskMB:   1024,
		})
		Ω(err).ShouldNot(HaveOccurred())

		return guid, fakeContainer
	}

	initNewContainer := func() (guid string, fakeContainer *wfakes.FakeContainer) {
		guid, fakeContainer = allocNewContainer()

		index := 13
		_, err := executorClient.InitializeContainer(guid, api.ContainerInitializationRequest{
			Log: models.LogConfig{
				Guid:       "the-app-guid",
				SourceName: "STG",
				Index:      &index,
			},
		})
		Ω(err).ShouldNot(HaveOccurred())

		return guid, fakeContainer
	}

	runTask := func(block bool) {
		guid, fakeContainer := initNewContainer()

		process := new(wfakes.FakeProcess)
		process.WaitStub = func() (int, error) {
			time.Sleep(time.Second)

			if block {
				select {}
			} else {
				_, io := fakeContainer.RunArgsForCall(0)

				Eventually(func() interface{} {
					_, err := io.Stdout.Write([]byte("some-output\n"))
					Ω(err).ShouldNot(HaveOccurred())

					return logConfirmations
				}).Should(Receive())
			}

			return 0, nil
		}

		fakeContainer.RunReturns(process, nil)

		err := executorClient.Run(guid, api.ContainerRunRequest{
			Actions: []models.ExecutorAction{
				{Action: models.RunAction{Path: "ls"}},
			},
		})
		Ω(err).ShouldNot(HaveOccurred())
	}

	Describe("starting up", func() {
		JustBeforeEach(func() {
			err := gardenServer.Start()
			Ω(err).ShouldNot(HaveOccurred())

			runner.StartWithoutCheck(executor_runner.Config{
				ContainerOwnerName: "executor-name",
			})
		})

		Context("when there are containers that are owned by the executor", func() {
			var fakeContainer1, fakeContainer2 *wfakes.FakeContainer

			BeforeEach(func() {
				fakeContainer1 = new(wfakes.FakeContainer)
				fakeContainer1.HandleReturns("handle-1")

				fakeContainer2 = new(wfakes.FakeContainer)
				fakeContainer2.HandleReturns("handle-2")

				fakeBackend.ContainersStub = func(ps warden.Properties) ([]warden.Container, error) {
					if reflect.DeepEqual(ps, warden.Properties{"owner": "executor-name"}) {
						return []warden.Container{fakeContainer1, fakeContainer2}, nil
					} else {
						return nil, nil
					}
				}
			})

			It("deletes those containers (and only those containers)", func() {
				Eventually(fakeBackend.DestroyCallCount).Should(Equal(2))
				Ω(fakeBackend.DestroyArgsForCall(0)).Should(Equal("handle-1"))
				Ω(fakeBackend.DestroyArgsForCall(1)).Should(Equal("handle-2"))
			})

			It("reports itself as started via logs", func() {
				Eventually(runner.Session).Should(gbytes.Say("executor.started"))
			})

			Context("when deleting the container fails", func() {
				BeforeEach(func() {
					fakeBackend.DestroyReturns(errors.New("i tried to delete the thing but i failed. sorry."))
				})

				It("should exit sadly", func() {
					Eventually(runner.Session).Should(gexec.Exit(1))
				})
			})
		})
	})

	Context("when started", func() {
		BeforeEach(func() {
			err := gardenServer.Start()
			Ω(err).ShouldNot(HaveOccurred())

			runner.Start(executor_runner.Config{
				DrainTimeout:            drainTimeout,
				RegistryPruningInterval: pruningInterval,
			})
		})

		Describe("pinging the server", func() {
			var err error

			JustBeforeEach(func() {
				err = executorClient.Ping()
			})

			Context("when Warden responds to ping", func() {
				BeforeEach(func() {
					fakeBackend.PingReturns(nil)
				})

				It("not return an error", func() {
					Ω(err).ShouldNot(HaveOccurred())
				})
			})

			Context("when Warden returns an error", func() {
				BeforeEach(func() {
					fakeBackend.PingReturns(errors.New("oh no!"))
				})

				It("should return an error", func() {
					Ω(err.Error()).Should(ContainSubstring("status: 502"))
				})
			})
		})

		Describe("initializing the container", func() {
			var initializeContainerRequest api.ContainerInitializationRequest
			var err error
			var initializedContainer api.Container
			var container *wfakes.FakeContainer
			var guid string

			JustBeforeEach(func() {
				initializedContainer, err = executorClient.InitializeContainer(guid, initializeContainerRequest)
			})

			BeforeEach(func() {
				guid, container = allocNewContainer()
				initializeContainerRequest = api.ContainerInitializationRequest{
					CpuPercent: 50.0,
				}
			})

			Context("when the requested CPU percent is > 100", func() {
				BeforeEach(func() {
					initializeContainerRequest = api.ContainerInitializationRequest{
						CpuPercent: 101.0,
					}
				})

				It("returns an error", func() {
					Ω(err).Should(HaveOccurred())
					Ω(err.Error()).Should(ContainSubstring("status: 400"))
				})
			})

			Context("when the requested CPU percent is < 0", func() {
				BeforeEach(func() {
					initializeContainerRequest = api.ContainerInitializationRequest{
						CpuPercent: -14.0,
					}
				})

				It("returns an error", func() {
					Ω(err).Should(HaveOccurred())
					Ω(err.Error()).Should(ContainSubstring("status: 400"))
				})
			})

			Context("when the container can be created", func() {
				BeforeEach(func() {
					fakeBackend.CreateReturns(container, nil)
				})

				It("doesn't return an error", func() {
					Ω(err).ShouldNot(HaveOccurred())
				})

				It("creates it with the configured owner", func() {
					created := fakeBackend.CreateArgsForCall(0)
					ownerName := fmt.Sprintf("executor-on-node-%d", config.GinkgoConfig.ParallelNode)
					Ω(created.Properties["owner"]).Should(Equal(ownerName))
				})

				It("applies the memory, disk, and cpu limits", func() {
					limitedMemory := container.LimitMemoryArgsForCall(0)
					Ω(limitedMemory.LimitInBytes).Should(Equal(uint64(1024 * 1024 * 1024)))

					limitedDisk := container.LimitDiskArgsForCall(0)
					Ω(limitedDisk.ByteHard).Should(Equal(uint64(1024 * 1024 * 1024)))

					limitedCPU := container.LimitCPUArgsForCall(0)
					Ω(limitedCPU.LimitInShares).Should(Equal(uint64(512)))
				})

				Context("when ports are exposed", func() {
					BeforeEach(func() {
						initializeContainerRequest = api.ContainerInitializationRequest{
							CpuPercent: 0.5,
							Ports: []api.PortMapping{
								{ContainerPort: 8080, HostPort: 0},
								{ContainerPort: 8081, HostPort: 1234},
							},
						}
					})

					It("exposes the configured ports", func() {
						netInH, netInC := container.NetInArgsForCall(0)
						Ω(netInH).Should(Equal(uint32(0)))
						Ω(netInC).Should(Equal(uint32(8080)))

						netInH, netInC = container.NetInArgsForCall(1)
						Ω(netInH).Should(Equal(uint32(1234)))
						Ω(netInC).Should(Equal(uint32(8081)))
					})

					Context("when net-in succeeds", func() {
						BeforeEach(func() {
							calls := uint32(0)
							container.NetInStub = func(uint32, uint32) (uint32, uint32, error) {
								calls++
								return 1234 * calls, 4567 * calls, nil
							}
						})

						It("returns the container with mapped ports", func() {
							Ω(initializedContainer.Ports).Should(Equal([]api.PortMapping{
								{HostPort: 1234, ContainerPort: 4567},
								{HostPort: 2468, ContainerPort: 9134},
							}))
						})
					})

					Context("when mapping the ports fails", func() {
						BeforeEach(func() {
							container.NetInReturns(0, 0, errors.New("oh no!"))
						})

						It("returns an error", func() {
							Ω(err.Error()).Should(ContainSubstring("status: 500"))
						})
					})
				})

				Context("when a zero-value CPU percentage is specified", func() {
					BeforeEach(func() {
						initializeContainerRequest = api.ContainerInitializationRequest{
							CpuPercent: 0,
						}
					})

					It("does not apply it", func() {
						Ω(container.LimitCPUCallCount()).Should(BeZero())
					})
				})

				Context("when limiting memory fails", func() {
					BeforeEach(func() {
						container.LimitMemoryReturns(errors.New("oh no!"))
					})

					It("returns an error", func() {
						Ω(err.Error()).Should(ContainSubstring("status: 500"))
					})
				})

				Context("when limiting disk fails", func() {
					BeforeEach(func() {
						container.LimitDiskReturns(errors.New("oh no!"))
					})

					It("returns an error", func() {
						Ω(err.Error()).Should(ContainSubstring("status: 500"))
					})
				})

				Context("when limiting CPU fails", func() {
					BeforeEach(func() {
						container.LimitCPUReturns(errors.New("oh no!"))
					})

					It("returns an error", func() {
						Ω(err.Error()).Should(ContainSubstring("status: 500"))
					})
				})
			})

			Context("when for some reason the container fails to create", func() {
				BeforeEach(func() {
					fakeBackend.CreateReturns(nil, errors.New("oh no!"))
				})

				It("returns an error", func() {
					Ω(err.Error()).Should(ContainSubstring("status: 500"))
				})
			})
		})

		Describe("running something", func() {
			It("streams log output", func() {
				runTask(false)

				message := &logmessage.LogMessage{}
				Eventually(logMessages, 5).Should(Receive(&message))

				Ω(message.GetAppId()).Should(Equal("the-app-guid"))
				Ω(message.GetSourceName()).Should(Equal("STG"))
				Ω(message.GetMessageType()).Should(Equal(logmessage.LogMessage_OUT))
				Ω(message.GetSourceId()).Should(Equal("13"))
				Ω(string(message.GetMessage())).Should(Equal("some-output"))
			})

			It("fails when the actions are invalid", func() {
				guid, _ := initNewContainer()
				err := executorClient.Run(
					guid,
					api.ContainerRunRequest{
						Actions: []models.ExecutorAction{
							{
								models.MonitorAction{
									HealthyHook: models.HealthRequest{
										URL: "some/bogus/url",
									},
									Action: models.ExecutorAction{
										models.RunAction{
											Path: "ls",
											Args: []string{"-al"},
										},
									},
								},
							},
						},
					},
				)
				Ω(err).Should(HaveOccurred())
			})

			Context("when there is a completeURL and metadata", func() {
				var callbackHandler *ghttp.Server
				var containerGuid string
				var fakeContainer *wfakes.FakeContainer

				BeforeEach(func() {
					callbackHandler = ghttp.NewServer()

					containerGuid, fakeContainer = initNewContainer()

					process := new(wfakes.FakeProcess)
					fakeContainer.RunReturns(process, nil)

					err := executorClient.Run(
						containerGuid,
						api.ContainerRunRequest{
							Actions: []models.ExecutorAction{
								{
									models.RunAction{
										Path: "ls",
										Args: []string{"-al"},
									},
								},
							},
							CompleteURL: callbackHandler.URL() + "/result",
						},
					)

					Ω(err).ShouldNot(HaveOccurred())
				})

				AfterEach(func() {
					callbackHandler.Close()
				})

				Context("and the completeURL succeeds", func() {
					BeforeEach(func() {
						callbackHandler.AppendHandlers(
							ghttp.CombineHandlers(
								ghttp.VerifyRequest("PUT", "/result"),
								ghttp.VerifyJSONRepresenting(api.ContainerRunResult{
									Guid:          containerGuid,
									Failed:        false,
									FailureReason: "",
									Result:        "",
								}),
							),
						)
					})

					It("invokes the callback with failed false", func() {
						Eventually(callbackHandler.ReceivedRequests).Should(HaveLen(1))
					})

					It("destroys the container and removes it from the registry", func() {
						Eventually(fakeBackend.DestroyCallCount).Should(Equal(1))
						Ω(fakeBackend.DestroyArgsForCall(0)).Should(Equal("some-handle"))
					})

					It("frees the container's reserved resources", func() {
						Eventually(executorClient.RemainingResources).Should(Equal(api.ExecutorResources{
							MemoryMB:   1024,
							DiskMB:     1024,
							Containers: 1024,
						}))
					})
				})

				Context("and the completeURL fails", func() {
					BeforeEach(func() {
						callbackHandler.AppendHandlers(
							http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								callbackHandler.HTTPTestServer.CloseClientConnections()
							}),
							ghttp.RespondWith(http.StatusInternalServerError, ""),
							ghttp.RespondWith(http.StatusOK, ""),
						)
					})

					It("invokes the callback repeatedly", func() {
						Eventually(callbackHandler.ReceivedRequests, 5).Should(HaveLen(3))
					})
				})
			})

			Context("when the actions fail", func() {
				var disaster = errors.New("because i said so")
				var fakeContainer *wfakes.FakeContainer
				var containerGuid string

				BeforeEach(func() {
					containerGuid, fakeContainer = initNewContainer()
					process := new(wfakes.FakeProcess)
					fakeContainer.RunReturns(process, nil)
					process.WaitReturns(1, disaster)
				})

				Context("and there is a completeURL", func() {
					var callbackHandler *ghttp.Server

					BeforeEach(func() {
						callbackHandler = ghttp.NewServer()

						callbackHandler.AppendHandlers(
							ghttp.CombineHandlers(
								ghttp.VerifyRequest("PUT", "/result"),
								ghttp.VerifyJSONRepresenting(api.ContainerRunResult{
									Guid:          containerGuid,
									Failed:        true,
									FailureReason: "process error: because i said so",
									Result:        "",
								}),
							),
						)

						err := executorClient.Run(
							containerGuid,
							api.ContainerRunRequest{
								Actions: []models.ExecutorAction{
									{
										models.RunAction{
											Path: "ls",
											Args: []string{"-al"},
										},
									},
								},
								CompleteURL: callbackHandler.URL() + "/result",
							},
						)

						Ω(err).ShouldNot(HaveOccurred())
					})

					AfterEach(func() {
						callbackHandler.Close()
					})

					It("invokes the callback with failed true and a reason", func() {
						Eventually(callbackHandler.ReceivedRequests).Should(HaveLen(1))
					})
				})
			})
		})

		Describe("deleting a container", func() {
			var err error
			var guid string

			JustBeforeEach(func() {
				err = executorClient.DeleteContainer(guid)
			})

			Context("when the container has been allocated", func() {
				BeforeEach(func() {
					guid, _ = allocNewContainer()
				})

				It("the previously allocated resources become available", func() {
					Eventually(executorClient.RemainingResources).Should(Equal(api.ExecutorResources{
						MemoryMB:   1024,
						DiskMB:     1024,
						Containers: 1024,
					}))
				})
			})

			Context("when the container has been initialized", func() {
				BeforeEach(func() {
					guid, _ = initNewContainer()
				})

				It("destroys the warden container", func() {
					Eventually(fakeBackend.DestroyCallCount).Should(Equal(1))
					Ω(fakeBackend.DestroyArgsForCall(0)).Should(Equal("some-handle"))
				})

				Describe("when deleting the container fails", func() {
					BeforeEach(func() {
						fakeBackend.DestroyReturns(errors.New("oh no!"))
					})

					It("returns an error", func() {
						Ω(err.Error()).Should(ContainSubstring("status: 500"))
					})
				})
			})
		})

		Describe("pruning the registry", func() {
			It("should prune the registry periodically", func() {
				It("continually prunes the registry", func() {
					guid, err := uuid.NewV4()
					Ω(err).ShouldNot(HaveOccurred())

					_, err = executorClient.AllocateContainer(guid.String(), api.ContainerAllocationRequest{
						MemoryMB: 1024,
						DiskMB:   1024,
					})
					Ω(err).ShouldNot(HaveOccurred())

					Ω(executorClient.ListContainers).Should(HaveLen(1))
					Eventually(executorClient.ListContainers, pruningInterval*2).Should(BeEmpty())
				})
			})
		})

		Describe("when the executor receives the TERM signal", func() {
			It("stops all running tasks", func() {
				runTask(true)
				runner.Session.Terminate()
				eventuallyContainerShouldBeDestroyed()
			})

			It("exits successfully", func() {
				runner.Session.Terminate()
				Eventually(runner.Session, 2*drainTimeout).Should(gexec.Exit(0))
			})
		})

		Describe("when the executor receives the INT signal", func() {
			It("stops all running tasks", func() {
				runTask(true)
				runner.Session.Interrupt()
				eventuallyContainerShouldBeDestroyed()
			})

			It("exits successfully", func() {
				runner.Session.Interrupt()
				Eventually(runner.Session, 2*drainTimeout).Should(gexec.Exit(0))
			})
		})

		Describe("when the executor receives the USR1 signal", func() {
			sendDrainSignal := func() {
				runner.Session.Signal(syscall.SIGUSR1)
				Eventually(runner.Session).Should(gbytes.Say("executor.draining"))
			}

			Context("when there are tasks running", func() {
				It("stops accepting requests", func() {
					sendDrainSignal()

					_, err := executorClient.AllocateContainer("container-123", api.ContainerAllocationRequest{})
					Ω(err).Should(HaveOccurred())
				})

				Context("and USR1 is received again", func() {
					It("does not die", func() {
						runTask(true)

						runner.Session.Signal(syscall.SIGUSR1)
						Eventually(runner.Session, time.Second).Should(gbytes.Say("executor.draining"))

						runner.Session.Signal(syscall.SIGUSR1)
						Eventually(runner.Session, 5*time.Second).Should(gbytes.Say("executor.signal.ignored"))
					})
				})

				Context("when the tasks complete before the drain timeout", func() {
					It("exits successfully", func() {
						runTask(false)

						sendDrainSignal()

						Eventually(runner.Session, drainTimeout-aBit).Should(gexec.Exit(0))
					})
				})

				Context("when the tasks do not complete before the drain timeout", func() {
					BeforeEach(func() {
						runTask(true)
					})

					It("cancels all running tasks", func() {
						sendDrainSignal()
						eventuallyContainerShouldBeDestroyed(drainTimeout + aBit)
					})

					It("exits successfully", func() {
						sendDrainSignal()
						Eventually(runner.Session, 2*drainTimeout).Should(gexec.Exit(0))
					})
				})
			})

			Context("when there are no tasks running", func() {
				It("exits successfully", func() {
					sendDrainSignal()
					Eventually(runner.Session).Should(gexec.Exit(0))
				})
			})
		})
	})
})

func startFakeLoggregatorServer(logConfirmations chan<- struct{}) (string, <-chan *logmessage.LogMessage) {
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	Ω(err).ShouldNot(HaveOccurred())

	conn, err := net.ListenUDP("udp", addr)
	Ω(err).ShouldNot(HaveOccurred())

	logMessages := make(chan *logmessage.LogMessage, 10)

	go func() {
		defer GinkgoRecover()

		for {
			buf := make([]byte, 1024)

			rlen, _, err := conn.ReadFromUDP(buf)
			logConfirmations <- struct{}{}
			Ω(err).ShouldNot(HaveOccurred())

			message, err := logmessage.ParseEnvelope(buf[0:rlen], "the-loggregator-secret")
			Ω(err).ShouldNot(HaveOccurred())

			logMessages <- message.GetLogMessage()
		}
	}()

	return conn.LocalAddr().String(), logMessages
}
