package clients

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	log "github.com/sirupsen/logrus"
)

const dockerResourcesLabel = "Fortify"

var labels = map[string]string{dockerResourcesLabel: "true"}

// DockerContainer is a resulting container reference, including the ID and configuration
type DockerContainer struct {
	ID        string
	ImageHash string
	Config    DockerContainerConfig
}

// DockerContainerConfig is configuration for a particular container
type DockerContainerConfig struct {
	Name           string
	Image          string
	Env            map[string]string
	LinkNetworkIDs []string
	NetworkID      string
	Ports          map[string]string
	Volumes        map[string]string
	Files          map[string][]byte
	MaxLogSize     string
	MaxLogFiles    int
}

// DockerClient is a client interface for interacting with docker
type DockerClient interface {
	CreatePublicNetwork(ctx context.Context, name string) (string, error)
	CreateInternalNetwork(ctx context.Context, name string) (string, error)
	AttachNetwork(ctx context.Context, containerID string, networkID string) error
	StartContainer(ctx context.Context, config DockerContainerConfig) (*DockerContainer, error)
	StopContainer(ctx context.Context, ID string) error
	Prune(ctx context.Context) error
}

type dockerClient struct {
}

func (cfg DockerContainerConfig) envVars() []string {
	var results []string
	for k, v := range cfg.Env {
		results = append(results, fmt.Sprintf("%s=%s", k, v))
	}
	return results
}

func (d *dockerClient) Prune(ctx context.Context) error {
	cli, err := client.NewClientWithOpts()
	if err != nil {
		return err
	}
	filter := filters.NewArgs(filters.Arg("label", dockerResourcesLabel))
	res, err := cli.NetworksPrune(ctx, filter)
	if err != nil {
		return err
	}
	for _, nw := range res.NetworksDeleted {
		log.Infof("pruned network %s", nw)
	}

	cpRes, err := cli.ContainersPrune(ctx, filter)
	if err != nil {
		return err
	}
	for _, cp := range cpRes.ContainersDeleted {
		log.Infof("pruned container %s", cp)
	}

	return nil
}

func (d *dockerClient) CreatePublicNetwork(ctx context.Context, name string) (string, error) {
	return d.createNetwork(ctx, name, false)
}

func (d *dockerClient) CreateInternalNetwork(ctx context.Context, name string) (string, error) {
	return d.createNetwork(ctx, name, true)
}

func (d *dockerClient) createNetwork(ctx context.Context, name string, internal bool) (string, error) {
	cli, err := client.NewClientWithOpts()
	if err != nil {
		return "", err
	}
	resp, err := cli.NetworkCreate(ctx, name, types.NetworkCreate{
		Labels:   labels,
		Internal: internal,
	})
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (d *dockerClient) AttachNetwork(ctx context.Context, containerID string, networkID string) error {
	cli, err := client.NewClientWithOpts()
	if err != nil {
		return err
	}
	return cli.NetworkConnect(ctx, networkID, containerID, nil)
}

func withTcp(port string) string {
	return fmt.Sprintf("%s/tcp", port)
}

// copyFile copies content bytes into container at /filename
func copyFile(cli *client.Client, ctx context.Context, filename string, content []byte, containerId string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := tw.WriteHeader(&tar.Header{
		Name: filename,
		Mode: 0400,
		Size: int64(len(content)),
	})
	if err != nil {
		return err
	}
	_, err = tw.Write(content)
	if err != nil {
		return err
	}
	err = tw.Close()
	if err != nil {
		return err
	}
	return cli.CopyToContainer(ctx, containerId, "/", &buf, types.CopyToContainerOptions{})
}

// StartContainer kicks off a container as a daemon and returns a summary of the container
func (d *dockerClient) StartContainer(ctx context.Context, config DockerContainerConfig) (*DockerContainer, error) {
	cli, err := client.NewClientWithOpts()
	if err != nil {
		return nil, err
	}

	bindings := make(map[nat.Port][]nat.PortBinding)
	ps := make(nat.PortSet)
	for hp, cp := range config.Ports {
		contPort := nat.Port(withTcp(cp))
		ps[contPort] = struct{}{}
		bindings[contPort] = []nat.PortBinding{{
			HostPort: hp,
			HostIP:   "0.0.0.0",
		}}
	}

	var volumes []string
	for hostVol, containerMnt := range config.Volumes {
		volumes = append(volumes, fmt.Sprintf("%s:%s", hostVol, containerMnt))
	}

	maxLogSize := config.MaxLogSize
	if maxLogSize == "" {
		maxLogSize = "10m"
	}

	maxLogFiles := config.MaxLogFiles
	if maxLogFiles == 0 {
		maxLogFiles = 10
	}

	cont, err := cli.ContainerCreate(
		ctx,
		&container.Config{
			Image:  config.Image,
			Env:    config.envVars(),
			Labels: labels,
		},
		&container.HostConfig{
			NetworkMode:     container.NetworkMode(config.NetworkID),
			PortBindings:    bindings,
			PublishAllPorts: true,
			Binds:           volumes,
			LogConfig: container.LogConfig{
				Config: map[string]string{
					"max-file": fmt.Sprintf("%d", maxLogFiles),
					"max-size": maxLogSize,
				},
				Type: "json-file",
			},
		}, nil, config.Name)

	if err != nil {
		return nil, err
	}

	for fn, b := range config.Files {
		if err := copyFile(cli, ctx, fn, b, cont.ID); err != nil {
			return nil, err
		}
	}

	if err := cli.ContainerStart(ctx, cont.ID, types.ContainerStartOptions{}); err != nil {
		return nil, err
	}

	for _, nwID := range config.LinkNetworkIDs {
		if err := d.AttachNetwork(ctx, cont.ID, nwID); err != nil {
			log.Error("error attaching network", err)
			return nil, err
		}
	}

	inspection, err := cli.ContainerInspect(ctx, cont.ID)
	if err != nil {
		return nil, err
	}

	log.Infof("Container %s (%s) is started", cont.ID, config.Name)
	return &DockerContainer{ID: cont.ID, Config: config, ImageHash: inspection.Image}, nil
}

// StopContainer kills a container by ID
func (d *dockerClient) StopContainer(ctx context.Context, ID string) error {
	cli, err := client.NewClientWithOpts()
	if err != nil {
		return err
	}
	return cli.ContainerKill(ctx, ID, "SIGKILL")
}

// NewDockerClient creates a new docker client
func NewDockerClient() *dockerClient {
	return &dockerClient{}
}
