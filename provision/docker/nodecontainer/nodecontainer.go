// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package nodecontainer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
	"sync"

	"github.com/fsouza/go-dockerclient"
	"github.com/tsuru/docker-cluster/cluster"
	"github.com/tsuru/tsuru/log"
	"github.com/tsuru/tsuru/net"
	"github.com/tsuru/tsuru/provision/docker/fix"
	"github.com/tsuru/tsuru/scopedconfig"
)

const (
	nodeContainerCollection = "nodeContainer"
)

type NodeContainerConfig struct {
	Name        string
	PinnedImage string
	Config      docker.Config
	HostConfig  docker.HostConfig
}

type NodeContainerConfigGroup struct {
	Name        string
	ConfigPools map[string]NodeContainerConfig
}

type NodeContainerConfigGroupSlice []NodeContainerConfigGroup

type ValidationErr struct {
	message string
}

func (n ValidationErr) Error() string {
	return n.message
}

func (l NodeContainerConfigGroupSlice) Len() int           { return len(l) }
func (l NodeContainerConfigGroupSlice) Swap(i, j int)      { l[i], l[j] = l[j], l[i] }
func (l NodeContainerConfigGroupSlice) Less(i, j int) bool { return l[i].Name < l[j].Name }

func (c *NodeContainerConfig) validate(pool string) error {
	if c.Name == "" {
		return ValidationErr{message: "node container config name cannot be empty"}
	}
	if c.Config.Image != "" && pool != "" {
		return ValidationErr{message: "it's not possible to override image in pool, please set image as a default value"}
	}
	return nil
}

func AddNewContainer(pool string, c *NodeContainerConfig) error {
	if err := c.validate(pool); err != nil {
		return err
	}
	conf := configFor(c.Name)
	return conf.Save(pool, c)
}

func UpdateContainer(pool string, c *NodeContainerConfig) error {
	if err := c.validate(pool); err != nil {
		return err
	}
	conf := configFor(c.Name)
	return conf.SaveMerge(pool, c)
}

func RemoveContainer(pool string, name string) error {
	conf := configFor(name)
	return conf.Remove(pool)
}

func LoadNodeContainer(pool string, name string) (*NodeContainerConfig, error) {
	conf := configFor(name)
	var result NodeContainerConfig
	err := conf.Load(pool, &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func LoadNodeContainersForPools(name string) (map[string]NodeContainerConfig, error) {
	return LoadNodeContainersForPoolsMerge(name, false)
}

func LoadNodeContainersForPoolsMerge(name string, merge bool) (map[string]NodeContainerConfig, error) {
	conf := configFor(name)
	var result map[string]NodeContainerConfig
	err := conf.LoadPoolsMerge(nil, &result, merge)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func AllNodeContainers() ([]NodeContainerConfigGroup, error) {
	confNames, err := scopedconfig.FindAllScopedConfigNames(nodeContainerCollection)
	if err != nil {
		return nil, err
	}
	result := make([]NodeContainerConfigGroup, len(confNames))
	for i, n := range confNames {
		confMap, err := LoadNodeContainersForPools(n)
		if err != nil {
			return nil, err
		}
		result[i] = NodeContainerConfigGroup{Name: n, ConfigPools: confMap}
	}
	return result, nil
}

// RecreateContainers relaunch all node containers in the cluster for the given
// DockerProvisioner, logging progress to the given writer.
//
// It assumes that the given writer is thread safe.
func RecreateContainers(p DockerProvisioner, w io.Writer, nodes ...cluster.Node) error {
	return ensureContainersStarted(p, w, true, nodes...)
}

func ensureContainersStarted(p DockerProvisioner, w io.Writer, relaunch bool, nodes ...cluster.Node) error {
	if w == nil {
		w = ioutil.Discard
	}
	confNames, err := scopedconfig.FindAllScopedConfigNames(nodeContainerCollection)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		nodes, err = p.Cluster().UnfilteredNodes()
		if err != nil {
			return err
		}
	}
	errChan := make(chan error, len(nodes))
	wg := sync.WaitGroup{}
	log.Debugf("[node containers] recreating %d containers", len(nodes))
	recreateContainer := func(node *cluster.Node, confName string) {
		defer wg.Done()
		pool := node.Metadata["pool"]
		containerConfig, confErr := LoadNodeContainer(pool, confName)
		if confErr != nil {
			errChan <- confErr
			return
		}
		log.Debugf("[node containers] recreating container %q in %s [%s]", confName, node.Address, pool)
		fmt.Fprintf(w, "relaunching node container %q in the node %s [%s]\n", confName, node.Address, pool)
		confErr = containerConfig.create(node.Address, pool, p, relaunch)
		if confErr != nil {
			msg := fmt.Sprintf("[node containers] failed to create container in %s [%s]: %s", node.Address, pool, confErr)
			log.Error(msg)
			errChan <- errors.New(msg)
		}
	}
	for i := range nodes {
		wg.Add(1)
		go func(node *cluster.Node) {
			defer wg.Done()
			for j := range confNames {
				wg.Add(1)
				go recreateContainer(node, confNames[j])
			}
		}(&nodes[i])
	}
	wg.Wait()
	close(errChan)
	var allErrors []string
	for err = range errChan {
		allErrors = append(allErrors, err.Error())
	}
	if len(allErrors) == 0 {
		return nil
	}
	return fmt.Errorf("multiple errors: %s", strings.Join(allErrors, ", "))
}

func (c *NodeContainerConfig) EnvMap() map[string]string {
	envMap := map[string]string{}
	for _, e := range c.Config.Env {
		parts := strings.SplitN(e, "=", 2)
		envMap[parts[0]] = parts[1]
	}
	return envMap
}

func (c *NodeContainerConfig) ResetImage() error {
	conf := configFor(c.Name)
	return conf.SetField("", "PinnedImage", "")
}

func (c *NodeContainerConfig) image() string {
	if c.PinnedImage != "" {
		return c.PinnedImage
	}
	return c.Config.Image
}

func (c *NodeContainerConfig) pullImage(client *docker.Client, p DockerProvisioner) (string, error) {
	image := c.image()
	output, err := pullWithRetry(client, p, image, 3)
	if err != nil {
		return "", err
	}
	var pinnedImage string
	if shouldPinImage(image) {
		digest, _ := fix.GetImageDigest(output)
		if digest != "" {
			pinnedImage = fmt.Sprintf("%s@%s", image, digest)
		}
		if pinnedImage != image {
			c.PinnedImage = pinnedImage
			conf := configFor(c.Name)
			err = conf.SetField("", "PinnedImage", pinnedImage)
		}
	}
	return image, err
}

func (c *NodeContainerConfig) create(dockerEndpoint, poolName string, p DockerProvisioner, relaunch bool) error {
	client, err := dockerClient(dockerEndpoint)
	if err != nil {
		return err
	}
	c.Config.Image, err = c.pullImage(client, p)
	if err != nil {
		return err
	}
	c.Config.Env = append([]string{"DOCKER_ENDPOINT=" + dockerEndpoint}, c.Config.Env...)
	opts := docker.CreateContainerOptions{
		Name:       c.Name,
		HostConfig: &c.HostConfig,
		Config:     &c.Config,
	}
	_, err = client.CreateContainer(opts)
	if relaunch && err == docker.ErrContainerAlreadyExists {
		err = client.RemoveContainer(docker.RemoveContainerOptions{ID: opts.Name, Force: true})
		if err != nil {
			return err
		}
		_, err = client.CreateContainer(opts)
	}
	if err != nil && err != docker.ErrContainerAlreadyExists {
		return err
	}
	err = client.StartContainer(c.Name, &c.HostConfig)
	if _, ok := err.(*docker.ContainerAlreadyRunning); !ok {
		return err
	}
	return nil
}

func configFor(name string) *scopedconfig.ScopedConfig {
	conf := scopedconfig.FindScopedConfigFor(nodeContainerCollection, name)
	conf.Jsonfy = true
	conf.SliceAdd = true
	conf.AllowMapEmpty = true
	return conf
}

func shouldPinImage(image string) bool {
	parts := strings.SplitN(image, "/", 3)
	lastPart := parts[len(parts)-1]
	versionParts := strings.SplitN(lastPart, ":", 2)
	return len(versionParts) < 2 || versionParts[1] == "latest"
}

func dockerClient(endpoint string) (*docker.Client, error) {
	client, err := docker.NewClient(endpoint)
	if err != nil {
		return nil, err
	}
	client.HTTPClient = net.Dial5Full300ClientNoKeepAlive
	client.Dialer = net.Dial5Dialer
	return client, nil
}

func pullWithRetry(client *docker.Client, p DockerProvisioner, image string, maxTries int) (string, error) {
	var buf bytes.Buffer
	var err error
	pullOpts := docker.PullImageOptions{Repository: image, OutputStream: &buf}
	registryAuth := p.RegistryAuthConfig()
	for ; maxTries > 0; maxTries-- {
		err = client.PullImage(pullOpts, registryAuth)
		if err == nil {
			return buf.String(), nil
		}
	}
	return "", err
}

type ClusterHook struct {
	Provisioner DockerProvisioner
}

func (h *ClusterHook) RunClusterHook(evt cluster.HookEvent, node *cluster.Node) error {
	_, err := InitializeBS()
	if err != nil {
		return fmt.Errorf("unable to initialize bs node container: %s", err)
	}
	err = ensureContainersStarted(h.Provisioner, nil, false, *node)
	if err != nil {
		return fmt.Errorf("unable to start node containers: %s", err)
	}
	return nil
}
