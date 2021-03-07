package room

import (
	"context"
	"fmt"
	"strings"
	"time"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	network "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	dockerClient "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"m1k1o/neko_rooms/internal/config"
	"m1k1o/neko_rooms/internal/types"
	"m1k1o/neko_rooms/internal/utils"
)

const (
	nekoImage       = "m1k1o/neko:latest"
	containerPrefix = "neko-room-"
	frontendPort    = 8080
	labelCanary     = "m1k1o-neko-rooms"
)

func New(config *config.Room) *RoomManagerCtx {
	logger := log.With().Str("module", "room").Logger()

	cli, err := dockerClient.NewEnvClient()
	if err != nil {
		logger.Panic().Err(err).Msg("unable to connect to docker client")
	} else {
		logger.Info().Msg("successfully connected to docker client")
	}

	return &RoomManagerCtx{
		logger: logger,
		config: config,
		client: cli,
	}
}

type RoomManagerCtx struct {
	logger zerolog.Logger
	config *config.Room
	client *dockerClient.Client
}

func (manager *RoomManagerCtx) List() ([]types.RoomEntry, error) {
	containers, err := manager.listContainers()
	if err != nil {
		return nil, err
	}

	result := []types.RoomEntry{}
	for _, container := range containers {
		roomName, ok := container.Labels["m1k1o.neko_rooms.name"]
		if !ok {
			return nil, fmt.Errorf("Damaged container labels: name not found.")
		}

		URL, ok := container.Labels["m1k1o.neko_rooms.url"]
		if !ok {
			return nil, fmt.Errorf("Damaged container labels: url not found.")
		}

		epr, err := manager.getEprFromLabels(container.Labels)
		if err != nil {
			return nil, err
		}

		result = append(result, types.RoomEntry{
			ID:             container.ID,
			URL:            URL,
			Name:           roomName,
			MaxConnections: epr.Max - epr.Min + 1,
			Image:          container.Image,
			Running:        container.State == "running",
			Status:         container.Status,
			Created:        time.Unix(container.Created, 0),
		})
	}

	return result, nil
}

func (manager *RoomManagerCtx) Create(settings types.RoomSettings) (string, error) {
	// TODO: Check if path name exists.
	roomName := settings.Name
	if roomName == "" {
		var err error
		roomName, err = utils.NewUID(32)
		if err != nil {
			return "", err
		}
	}

	epr, err := manager.allocatePorts(settings.MaxConnections)
	if err != nil {
		return "", err
	}

	portBindings := nat.PortMap{}
	exposedPorts := nat.PortSet{
		nat.Port(fmt.Sprintf("%d/udp", frontendPort)): struct{}{},
	}

	for port := epr.Min; port <= epr.Max; port++ {
		portKey := nat.Port(fmt.Sprintf("%d/udp", port))

		portBindings[portKey] = []nat.PortBinding{
			{
				HostIP:   "0.0.0.0",
				HostPort: fmt.Sprintf("%d", port),
			},
		}

		exposedPorts[portKey] = struct{}{}
	}

	containerName := containerPrefix + roomName

	urlProto := "http"
	if manager.config.TraefikCertresolver != "" {
		urlProto = "https"
	}

	labels := map[string]string{
		// Set internal labels
		"m1k1o.neko_rooms.name":    roomName,
		"m1k1o.neko_rooms.url":     urlProto + "://" + manager.config.TraefikDomain + "/" + roomName + "/",
		"m1k1o.neko_rooms.canary":  labelCanary,
		"m1k1o.neko_rooms.epr.min": fmt.Sprintf("%d", epr.Min),
		"m1k1o.neko_rooms.epr.max": fmt.Sprintf("%d", epr.Max),

		// Set traefik labels
		"traefik.enable": "true",
		"traefik.http.services." + containerName + "-frontend.loadbalancer.server.port": fmt.Sprintf("%d", frontendPort),
		"traefik.http.routers." + containerName + ".entrypoints":                        manager.config.TraefikEntrypoint,
		"traefik.http.routers." + containerName + ".rule":                               "Host(`" + manager.config.TraefikDomain + "`) && PathPrefix(`/" + roomName + "`)",
		"traefik.http.middlewares." + containerName + "-rdr.redirectregex.regex":        "/" + roomName + "$$",
		"traefik.http.middlewares." + containerName + "-rdr.redirectregex.replacement":  "/" + roomName + "/",
		"traefik.http.middlewares." + containerName + "-prf.stripprefix.prefixes":       "/" + roomName + "/",
		"traefik.http.routers." + containerName + ".middlewares":                        containerName + "-rdr," + containerName + "-prf",
	}

	// optional HTTPS
	if manager.config.TraefikCertresolver != "" {
		labels["traefik.http.routers."+containerName+".tls"] = "true"
		labels["traefik.http.routers."+containerName+".tls.certresolver"] = manager.config.TraefikCertresolver
	}

	config := &container.Config{
		// Hostname
		Hostname: containerName,
		// Domainname
		Domainname: containerName,
		// List of exposed ports
		ExposedPorts: exposedPorts,
		// List of environment variable to set in the container
		Env: append([]string{
			fmt.Sprintf("NEKO_BIND=%d", frontendPort),
			fmt.Sprintf("NEKO_EPR=%d-%d", epr.Min, epr.Max),
			fmt.Sprintf("NEKO_NAT1TO1=%s", strings.Join(manager.config.NAT1To1IPs, ",")),
		}, settings.ToEnv()...),
		// Name of the image as it was passed by the operator (e.g. could be symbolic)
		Image: nekoImage,
		// List of labels set to this container
		Labels: labels,
	}

	hostConfig := &container.HostConfig{
		// Port mapping between the exposed port (container) and the host
		PortBindings: portBindings,
		// Configuration of the logs for this container
		LogConfig: container.LogConfig{
			Type:   "json-file",
			Config: map[string]string{},
		},
		// Restart policy to be used for the container
		RestartPolicy: container.RestartPolicy{
			Name: "always",
		},
		// List of kernel capabilities to add to the container
		CapAdd: strslice.StrSlice{
			"SYS_ADMIN",
		},
		// Total shm memory usage
		ShmSize: 2 * 10e9,
	}

	networkingConfig := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			manager.config.TraefikNetwork: &network.EndpointSettings{},
		},
	}

	// Creating the actual container
	cont, err := manager.client.ContainerCreate(
		context.Background(),
		config,
		hostConfig,
		networkingConfig,
		nil,
		containerName,
	)

	if err != nil {
		return "", err
	}

	// Start the actual container
	err = manager.client.ContainerStart(context.Background(), cont.ID, dockerTypes.ContainerStartOptions{})

	if err != nil {
		return "", err
	}

	return cont.ID, nil
}

func (manager *RoomManagerCtx) Get(id string) (*types.RoomSettings, error) {
	container, err := manager.inspectContainer(id)
	if err != nil {
		return nil, err
	}

	roomName, ok := container.Config.Labels["m1k1o.neko_rooms.name"]
	if !ok {
		return nil, fmt.Errorf("Damaged container labels: name not found.")
	}

	epr, err := manager.getEprFromLabels(container.Config.Labels)
	if err != nil {
		return nil, err
	}

	settings := types.RoomSettings{
		Name:           roomName,
		MaxConnections: epr.Max - epr.Min + 1,
	}

	err = settings.FromEnv(container.Config.Env)
	return &settings, err
}

func (manager *RoomManagerCtx) Remove(id string) error {
	_, err := manager.inspectContainer(id)
	if err != nil {
		return err
	}

	// Stop the actual container
	err = manager.client.ContainerStop(context.Background(), id, nil)

	if err != nil {
		return err
	}

	// Remove the actual container
	err = manager.client.ContainerRemove(context.Background(), id, dockerTypes.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	})

	return err
}

func (manager *RoomManagerCtx) Start(id string) error {
	_, err := manager.inspectContainer(id)
	if err != nil {
		return err
	}

	// Start the actual container
	return manager.client.ContainerStart(context.Background(), id, dockerTypes.ContainerStartOptions{})
}


func (manager *RoomManagerCtx) Stop(id string) error {
	_, err := manager.inspectContainer(id)
	if err != nil {
		return err
	}

	// Stop the actual container
	return manager.client.ContainerStop(context.Background(), id, nil)
}

func (manager *RoomManagerCtx) Restart(id string) error {
	_, err := manager.inspectContainer(id)
	if err != nil {
		return err
	}

	// Restart the actual container
	return manager.client.ContainerRestart(context.Background(), id, nil)
}
