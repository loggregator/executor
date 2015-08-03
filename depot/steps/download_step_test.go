package steps_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"reflect"

	"github.com/cloudfoundry-incubator/cacheddownloader"
	cdfakes "github.com/cloudfoundry-incubator/cacheddownloader/fakes"
	"github.com/pivotal-golang/lager/lagertest"

	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/cloudfoundry-incubator/garden"

	"github.com/cloudfoundry-incubator/executor/depot/log_streamer/fake_log_streamer"
	"github.com/cloudfoundry-incubator/executor/depot/steps"
	"github.com/cloudfoundry-incubator/executor/fakes"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"

	archiveHelper "github.com/pivotal-golang/archiver/extractor/test_helper"
)

var _ = Describe("DownloadAction", func() {
	var (
		step steps.Step

		downloadAction models.DownloadAction
		cache          *cdfakes.FakeCachedDownloader
		gardenClient   *fakes.FakeGardenClient
		fakeStreamer   *fake_log_streamer.FakeLogStreamer
		logger         *lagertest.TestLogger
		rateLimiter    chan struct{}

		allowPrivileged bool
	)

	handle := "some-container-handle"

	BeforeEach(func() {
		cache = &cdfakes.FakeCachedDownloader{}
		cache.FetchReturns(ioutil.NopCloser(new(bytes.Buffer)), 42, nil)

		downloadAction = models.DownloadAction{
			From:     "http://mr_jones",
			To:       "/tmp/Antarctica",
			CacheKey: "the-cache-key",
			User:     "notroot",
		}

		gardenClient = fakes.NewGardenClient()

		fakeStreamer = newFakeStreamer()
		logger = lagertest.NewTestLogger("test")

		rateLimiter = make(chan struct{}, 1)
	})

	Describe("Perform", func() {
		var stepErr error

		JustBeforeEach(func() {
			container, err := gardenClient.Create(garden.ContainerSpec{
				Handle: handle,
			})
			Expect(err).NotTo(HaveOccurred())

			step = steps.NewDownload(
				container,
				downloadAction,
				cache,
				rateLimiter,
				allowPrivileged,
				fakeStreamer,
				logger,
			)

			stepErr = step.Perform()
		})

		var tarReader *tar.Reader

		It("downloads via the cache with a tar transformer", func() {
			Expect(cache.FetchCallCount()).To(Equal(1))

			url, cacheKey, transformer, cancelChan := cache.FetchArgsForCall(0)
			Expect(url.Host).To(ContainSubstring("mr_jones"))
			Expect(cacheKey).To(Equal("the-cache-key"))
			Expect(cancelChan).NotTo(BeNil())

			tVal := reflect.ValueOf(transformer)
			expectedVal := reflect.ValueOf(cacheddownloader.TarTransform)

			Expect(tVal.Pointer()).To(Equal(expectedVal.Pointer()))
		})

		It("logs the step", func() {
			Expect(logger.TestSink.LogMessages()).To(ConsistOf([]string{
				"test.download-step.fetch-starting",
				"test.download-step.fetch-complete",
				"test.download-step.stream-in-starting",
				"test.download-step.stream-in-complete",
			}))
		})

		Context("when the action downloads as root", func() {
			BeforeEach(func() {
				downloadAction.User = "root"
			})

			Context("with allowPrivileged set to false", func() {
				BeforeEach(func() {
					allowPrivileged = false
				})

				It("errors when trying to execute a download action as root", func() {
					Expect(stepErr).To(HaveOccurred())
				})

				It("logs the step", func() {
					Expect(logger.TestSink.LogMessages()).To(ConsistOf([]string{
						"test.download-step.privileged-action-denied",
					}))
				})
			})

			Context("with allowPrivileged set to true", func() {
				BeforeEach(func() {
					allowPrivileged = true
				})

				It("does not error when trying to execute a download action as root", func() {
					Expect(stepErr).NotTo(HaveOccurred())
				})

				It("streams in as root", func() {
					_, spec := gardenClient.Connection.StreamInArgsForCall(0)
					Expect(spec.User).To(Equal("root"))
				})
			})
		})

		Context("when an artifact is not specified", func() {
			It("does not stream the download information", func() {
				err := step.Perform()
				Expect(err).NotTo(HaveOccurred())

				stdout := fakeStreamer.Stdout().(*gbytes.Buffer)
				Expect(stdout.Contents()).To(BeEmpty())
			})
		})

		Context("when an artifact is specified", func() {
			BeforeEach(func() {
				downloadAction.Artifact = "artifact"
			})

			Describe("logging the size", func() {
				Context("when nothing had to be downloaded", func() {
					BeforeEach(func() {
						cache.FetchReturns(gbytes.NewBuffer(), 0, nil) // 0 bytes downlaoded
					})

					It("streams unknown when the Fetch does not return a File", func() {
						Expect(stepErr).NotTo(HaveOccurred())

						stdout := fakeStreamer.Stdout().(*gbytes.Buffer)
						Expect(stdout.Contents()).To(ContainSubstring("Downloaded artifact\n"))
					})
				})

				Context("when data was downloaded", func() {
					BeforeEach(func() {
						cache.FetchReturns(gbytes.NewBuffer(), 42, nil)
					})

					It("streams the size when the Fetch returns a File", func() {
						Expect(stepErr).NotTo(HaveOccurred())

						stdout := fakeStreamer.Stdout().(*gbytes.Buffer)
						Expect(stdout.Contents()).To(ContainSubstring("Downloaded artifact (42B)"))
					})
				})
			})
		})

		Context("when there is an error parsing the download url", func() {
			BeforeEach(func() {
				downloadAction.From = "foo/bar"
			})

			It("returns an error", func() {
				Expect(stepErr).To(HaveOccurred())
			})

			It("logs the step", func() {
				Expect(logger.TestSink.LogMessages()).To(ConsistOf([]string{
					"test.download-step.fetch-starting",
					"test.download-step.parse-request-uri-error",
				}))

			})
		})

		Context("and the fetched bits are a valid tarball", func() {
			BeforeEach(func() {
				tarFile := createTempTar()
				defer os.Remove(tarFile.Name())

				cache.FetchReturns(tarFile, 42, nil)
			})

			Context("and streaming in succeeds", func() {
				BeforeEach(func() {
					buffer := &bytes.Buffer{}
					tarReader = tar.NewReader(buffer)

					gardenClient.Connection.StreamInStub = func(handle string, spec garden.StreamInSpec) error {
						Expect(spec.Path).To(Equal("/tmp/Antarctica"))
						Expect(spec.User).To(Equal("notroot"))

						_, err := io.Copy(buffer, spec.TarStream)
						Expect(err).NotTo(HaveOccurred())

						return nil
					}
				})

				It("does not return an error", func() {
					Expect(stepErr).NotTo(HaveOccurred())
				})

				It("places the file in the container under the destination", func() {
					header, err := tarReader.Next()
					Expect(err).NotTo(HaveOccurred())
					Expect(header.Name).To(Equal("file1"))
				})
			})

			Context("when there is an error copying the extracted files into the container", func() {
				var expectedErr = errors.New("oh no!")

				BeforeEach(func() {
					gardenClient.Connection.StreamInReturns(expectedErr)
				})

				It("returns an error", func() {
					Expect(stepErr.Error()).To(ContainSubstring("Copying into the container failed"))
				})

				It("logs the step", func() {
					Expect(logger.TestSink.LogMessages()).To(ConsistOf([]string{
						"test.download-step.fetch-starting",
						"test.download-step.fetch-complete",
						"test.download-step.stream-in-starting",
						"test.download-step.stream-in-failed",
					}))

				})
			})
		})

		Context("when there is an error fetching the file", func() {
			BeforeEach(func() {
				cache.FetchReturns(nil, 0, errors.New("oh no!"))
			})

			It("returns an error", func() {
				Expect(stepErr.Error()).To(ContainSubstring("Downloading failed"))
			})

			It("logs the step", func() {
				Expect(logger.TestSink.LogMessages()).To(ConsistOf([]string{
					"test.download-step.fetch-starting",
					"test.download-step.fetch-failed",
				}))

			})
		})
	})

	Describe("Cancel", func() {
		var result chan error

		BeforeEach(func() {
			result = make(chan error)

			container, err := gardenClient.Create(garden.ContainerSpec{
				Handle: handle,
			})
			Expect(err).NotTo(HaveOccurred())

			step = steps.NewDownload(
				container,
				downloadAction,
				cache,
				rateLimiter,
				allowPrivileged,
				fakeStreamer,
				logger,
			)
		})

		Context("when waiting on the rate limiter", func() {
			JustBeforeEach(func() {
				rateLimiter <- struct{}{}
				go func() { result <- step.Perform() }()
			})

			It("cancels the wait", func() {
				step.Cancel()
				Eventually(result).Should(Receive(Equal(steps.ErrCancelled)))
			})

			It("does not fetch the download artifact", func() {
				step.Cancel()
				Eventually(result).Should(Receive(Equal(steps.ErrCancelled)))
				Expect(cache.FetchCallCount()).To(Equal(0))
			})
		})

		Context("when downloading the file", func() {
			var calledChan chan struct{}

			BeforeEach(func() {
				calledChan = make(chan struct{})

				cache.FetchStub = func(u *url.URL, key string, t cacheddownloader.CacheTransformer, cancelCh <-chan struct{}) (io.ReadCloser, int64, error) {
					Expect(cancelCh).NotTo(BeNil())
					Expect(cancelCh).NotTo(BeClosed())

					close(calledChan)
					<-cancelCh

					Expect(cancelCh).To(BeClosed())

					return nil, 0, errors.New("some error indicating a cancel")
				}
			})

			JustBeforeEach(func() {
				go func() { result <- step.Perform() }()
			})

			It("closes the cancel channel and propagates the cancel error", func() {
				Eventually(calledChan).Should(BeClosed())
				step.Cancel()

				Eventually(result).Should(Receive(Equal(steps.ErrCancelled)))
			})
		})

		Context("when streaming the file into the container", func() {
			var calledChan chan struct{}
			var barrierChan chan struct{}

			BeforeEach(func() {
				tarFile := createTempTar()
				defer os.Remove(tarFile.Name())
				cache.FetchReturns(tarFile, 0, nil)

				calledChan = make(chan struct{})
				barrierChan = make(chan struct{})

				gardenClient.Connection.StreamInStub = func(handle string, spec garden.StreamInSpec) error {
					writer := func(p []byte) (n int, err error) {
						close(calledChan)
						<-barrierChan
						return 1, nil
					}
					_, err := io.Copy(WriteFunc(writer), spec.TarStream)
					return err
				}
			})

			JustBeforeEach(func() {
				go func() { result <- step.Perform() }()
			})

			It("aborts the streaming", func() {
				Eventually(calledChan).Should(BeClosed())
				step.Cancel()
				close(barrierChan)

				Eventually(result).Should(Receive(Equal(steps.ErrCancelled)))
			})
		})
	})

	Describe("the downloads are rate limited", func() {
		var container garden.Container

		BeforeEach(func() {
			var err error
			container, err = gardenClient.Create(garden.ContainerSpec{
				Handle: handle,
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("allows only N concurrent downloads", func() {
			rateLimiter := make(chan struct{}, 2)

			downloadAction1 := models.DownloadAction{
				From: "http://mr_jones1",
				To:   "/tmp/Antarctica",
			}

			step1 := steps.NewDownload(
				container,
				downloadAction1,
				cache,
				rateLimiter,
				allowPrivileged,
				fakeStreamer,
				logger,
			)

			downloadAction2 := models.DownloadAction{
				From: "http://mr_jones2",
				To:   "/tmp/Antarctica",
			}

			step2 := steps.NewDownload(
				container,
				downloadAction2,
				cache,
				rateLimiter,
				allowPrivileged,
				fakeStreamer,
				logger,
			)

			downloadAction3 := models.DownloadAction{
				From: "http://mr_jones3",
				To:   "/tmp/Antarctica",
			}

			step3 := steps.NewDownload(
				container,
				downloadAction3,
				cache,
				rateLimiter,
				allowPrivileged,
				fakeStreamer,
				logger,
			)

			fetchCh := make(chan struct{}, 3)
			barrier := make(chan struct{})
			nopCloser := ioutil.NopCloser(new(bytes.Buffer))
			cache.FetchStub = func(urlToFetch *url.URL, cacheKey string, transformer cacheddownloader.CacheTransformer, cancelChan <-chan struct{}) (io.ReadCloser, int64, error) {
				fetchCh <- struct{}{}
				<-barrier
				return nopCloser, 42, nil
			}

			go func() {
				defer GinkgoRecover()

				err := step1.Perform()
				Expect(err).NotTo(HaveOccurred())
			}()
			go func() {
				defer GinkgoRecover()

				err := step2.Perform()
				Expect(err).NotTo(HaveOccurred())
			}()
			go func() {
				defer GinkgoRecover()

				err := step3.Perform()
				Expect(err).NotTo(HaveOccurred())
			}()

			Eventually(fetchCh).Should(Receive())
			Eventually(fetchCh).Should(Receive())
			Consistently(fetchCh).ShouldNot(Receive())

			barrier <- struct{}{}

			Eventually(fetchCh).Should(Receive())

			close(barrier)
		})
	})
})

func createTempTar() *os.File {
	tarFile, err := ioutil.TempFile("", "some-tar")
	Expect(err).NotTo(HaveOccurred())

	archiveHelper.CreateTarArchive(
		tarFile.Name(),
		[]archiveHelper.ArchiveFile{{Name: "file1"}},
	)

	_, err = tarFile.Seek(0, 0)
	Expect(err).NotTo(HaveOccurred())

	return tarFile
}

type WriteFunc func(p []byte) (n int, err error)

func (wf WriteFunc) Write(p []byte) (n int, err error) {
	return wf(p)
}
