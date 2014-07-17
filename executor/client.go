package executor

import (
	"errors"

	"github.com/cloudfoundry-incubator/executor/api"
	"github.com/cloudfoundry-incubator/executor/registry"
	"github.com/cloudfoundry-incubator/executor/sequence"
	"github.com/cloudfoundry-incubator/executor/transformer"
	"github.com/cloudfoundry-incubator/garden/warden"
	"github.com/cloudfoundry-incubator/runtime-schema/models"
	"github.com/cloudfoundry/gosteno"
)

var (
	ContainerNotFound = errors.New("container not found")
	StepsInvalid      = errors.New("steps invalid")
	LimitsInvalid     = errors.New("container limits invalid")
)

type Client interface {
	InitializeContainer(guid string, request api.ContainerInitializationRequest) (api.Container, error)
	RunContainer(guid string, actions []models.ExecutorAction, completeURL string) error
	DeleteContainer(guid string) error
	Ping() error
}

type client struct {
	containerOwnerName    string
	containerMaxCPUShares uint64
	wardenClient          warden.Client
	registry              registry.Registry
	transformer           *transformer.Transformer
	runActions            chan<- DepotRunAction
	logger                *gosteno.Logger
}

func NewClient(
	containerOwnerName string,
	containerMaxCPUShares uint64,
	wardenClient warden.Client,
	registry registry.Registry,
	transformer *transformer.Transformer,
	runActions chan<- DepotRunAction,
	logger *gosteno.Logger,
) Client {
	return &client{
		containerOwnerName:    containerOwnerName,
		containerMaxCPUShares: containerMaxCPUShares,
		wardenClient:          wardenClient,
		registry:              registry,
		transformer:           transformer,
		runActions:            runActions,
		logger:                logger,
	}
}

func (c *client) InitializeContainer(guid string, request api.ContainerInitializationRequest) (api.Container, error) {
	if request.CpuPercent > 100 || request.CpuPercent < 0 {
		return api.Container{}, LimitsInvalid
	}

	reg, err := c.registry.FindByGuid(guid)
	if err != nil {
		c.logger.Infod(map[string]interface{}{
			"error": err.Error(),
		}, "executor.init-container.not-found")
		return api.Container{}, ContainerNotFound
	}

	containerClient, err := c.wardenClient.Create(warden.ContainerSpec{
		Properties: warden.Properties{
			"owner": c.containerOwnerName,
		},
	})
	if err != nil {
		c.logger.Errord(map[string]interface{}{
			"error": err.Error(),
		}, "executor.init-container.create-failed")
		return api.Container{}, err
	}

	err = c.limitContainerDiskAndMemory(reg, containerClient)
	if err != nil {
		c.logger.Errord(map[string]interface{}{
			"error": err.Error(),
		}, "executor.init-container.limit-disk-and-memory-failed")
		return api.Container{}, err
	}

	err = c.limitContainerCPU(request, containerClient)
	if err != nil {
		c.logger.Errord(map[string]interface{}{
			"error": err.Error(),
		}, "executor.init-container.limit-cpu-failed")
		return api.Container{}, err
	}

	portMapping, err := c.mapPorts(request, containerClient)
	if err != nil {
		c.logger.Errord(map[string]interface{}{
			"error": err.Error(),
		}, "executor.init-container.port-mapping-failed")
		return api.Container{}, err
	}

	request.Ports = portMapping

	reg, err = c.registry.Create(reg.Guid, containerClient.Handle(), request)
	if err != nil {
		c.logger.Errord(map[string]interface{}{
			"error": err.Error(),
		}, "executor.init-container.registry-failed")
		return api.Container{}, err
	}

	return reg, nil
}

func (c *client) limitContainerDiskAndMemory(reg api.Container, containerClient warden.Container) error {
	if reg.MemoryMB != 0 {
		err := containerClient.LimitMemory(warden.MemoryLimits{
			LimitInBytes: uint64(reg.MemoryMB * 1024 * 1024),
		})
		if err != nil {
			return err
		}
	}

	if reg.DiskMB != 0 {
		err := containerClient.LimitDisk(warden.DiskLimits{
			ByteHard: uint64(reg.DiskMB * 1024 * 1024),
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *client) limitContainerCPU(request api.ContainerInitializationRequest, containerClient warden.Container) error {
	if request.CpuPercent != 0 {
		err := containerClient.LimitCPU(warden.CPULimits{
			LimitInShares: uint64(float64(c.containerMaxCPUShares) * float64(request.CpuPercent) / 100.0),
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *client) mapPorts(request api.ContainerInitializationRequest, containerClient warden.Container) ([]api.PortMapping, error) {
	var result []api.PortMapping
	for _, mapping := range request.Ports {
		hostPort, containerPort, err := containerClient.NetIn(mapping.HostPort, mapping.ContainerPort)
		if err != nil {
			return nil, err
		}

		result = append(result, api.PortMapping{
			HostPort:      hostPort,
			ContainerPort: containerPort,
		})
	}

	return result, nil
}

func (c *client) RunContainer(guid string, actions []models.ExecutorAction, completeURL string) error {
	registration, err := c.registry.FindByGuid(guid)
	if err != nil {
		c.logger.Infod(map[string]interface{}{
			"error": err.Error(),
		}, "executor.run-actions.container-not-found")
		return ContainerNotFound
	}

	container, err := c.wardenClient.Lookup(registration.ContainerHandle)
	if err != nil {
		c.logger.Infod(map[string]interface{}{
			"error": err.Error(),
		}, "executor.run-actions.lookup-failed")
		return err
	}

	var result string
	steps, err := c.transformer.StepsFor(registration.Log, actions, container, &result)
	if err != nil {
		c.logger.Warnd(map[string]interface{}{
			"error": err.Error(),
		}, "executor.run-actions.steps-invalid")
		return StepsInvalid
	}

	c.runActions <- DepotRunAction{
		CompleteURL:  completeURL,
		Registration: registration,
		Sequence:     sequence.New(steps),
		Result:       &result,
	}

	return nil
}

func (c *client) DeleteContainer(guid string) error {
	registration, err := c.registry.FindByGuid(guid)
	if err != nil {
		return handleDeleteError(err, c.logger)
	}

	//TODO once wardenClient has an ErrNotFound error code, use it
	//to determine if we should delete from registry
	if registration.ContainerHandle != "" {
		err = c.wardenClient.Destroy(registration.ContainerHandle)
		if err != nil {
			return handleDeleteError(err, c.logger)
		}
	}

	err = c.registry.Delete(guid)
	if err != nil {
		return handleDeleteError(err, c.logger)
	}

	return nil
}

func (c *client) Ping() error {
	return c.wardenClient.Ping()
}

func handleDeleteError(err error, logger *gosteno.Logger) error {
	if err == registry.ErrContainerNotFound {
		logger.Infod(map[string]interface{}{
			"error": err.Error(),
		}, "executor.delete-container.not-found")
		return ContainerNotFound
	}

	logger.Errord(map[string]interface{}{
		"error": err.Error(),
	}, "executor.delete-container.failed")
	return err
}
