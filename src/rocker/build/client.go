/*-
 * Copyright 2015 Grammarly, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package build

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"

	"regexp"
	"rocker/dockerclient"
	"rocker/imagename"
	"rocker/textformatter"

	"github.com/docker/docker/pkg/units"

	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/term"
	"github.com/fsouza/go-dockerclient"
	"github.com/kr/pretty"

	"github.com/Sirupsen/logrus"
)

// Client interface
type Client interface {
	InspectImage(name string) (*docker.Image, error)
	PullImage(name string) error
	ListImages() (images []*imagename.ImageName, err error)
	ListImageTags(name string) (images []*imagename.ImageName, err error)
	RemoveImage(imageID string) error
	TagImage(imageID, imageName string) error
	PushImage(imageName string) (digest string, err error)
	EnsureImage(imageName string) error
	CreateContainer(state State) (id string, err error)
	RunContainer(containerID string, attachStdin bool) error
	CommitContainer(state State, message string) (img *docker.Image, err error)
	RemoveContainer(containerID string) error
	UploadToContainer(containerID string, stream io.Reader, path string) error
	EnsureContainer(containerName string, config *docker.Config, purpose string) (containerID string, err error)
	InspectContainer(containerName string) (*docker.Container, error)
	ResolveHostPath(path string) (resultPath string, err error)
}

// DockerClient implements the client that works with a docker socket
type DockerClient struct {
	client *docker.Client
	auth   docker.AuthConfiguration
	log    *logrus.Logger
}

var (
	captureDigest = regexp.MustCompile("digest:\\s*(sha256:[a-f0-9]{64})")
)

// NewDockerClient makes a new client that works with a docker socket
func NewDockerClient(dockerClient *docker.Client, auth docker.AuthConfiguration, log *logrus.Logger) *DockerClient {
	if log == nil {
		log = logrus.StandardLogger()
	}
	return &DockerClient{
		client: dockerClient,
		auth:   auth,
		log:    log,
	}
}

// InspectImage inspects docker image
// it does not give an error when image not found, but returns nil instead
func (c *DockerClient) InspectImage(name string) (img *docker.Image, err error) {
	// We simply return nil in case image not found
	if img, err = c.client.InspectImage(name); err == docker.ErrNoSuchImage {
		return nil, nil
	}
	return img, err
}

// PullImage pulls docker image
func (c *DockerClient) PullImage(name string) error {

	var (
		image                  = imagename.NewFromString(name)
		pipeReader, pipeWriter = io.Pipe()
		fdOut, isTerminalOut   = term.GetFdInfo(c.log.Out)
		out                    = c.log.Out
		errch                  = make(chan error, 1)
	)

	if !isTerminalOut {
		out = c.log.Writer()
	}

	opts := docker.PullImageOptions{
		Repository:    image.NameWithRegistry(),
		Registry:      image.Registry,
		Tag:           image.GetTag(),
		OutputStream:  pipeWriter,
		RawJSONStream: true,
	}

	c.log.Infof("| Pull image %s", image)
	c.log.Debugf("Pull image %s with options: %# v", image, opts)

	go func() {
		errch <- jsonmessage.DisplayJSONMessagesStream(pipeReader, out, fdOut, isTerminalOut)
	}()

	if err := c.client.PullImage(opts, c.auth); err != nil {
		return err
	}

	return <-errch
}

// ListImages lists all pulled images in the local docker registry
func (c *DockerClient) ListImages() (images []*imagename.ImageName, err error) {

	var dockerImages []docker.APIImages
	if dockerImages, err = c.client.ListImages(docker.ListImagesOptions{}); err != nil {
		return
	}

	images = []*imagename.ImageName{}
	for _, image := range dockerImages {
		for _, repoTag := range image.RepoTags {
			images = append(images, imagename.NewFromString(repoTag))
		}
	}

	return
}

// ListImageTags returns the list of images instances obtained from all tags existing in the registry
func (c *DockerClient) ListImageTags(name string) (images []*imagename.ImageName, err error) {
	return imagename.RegistryListTags(imagename.NewFromString(name))
}

// RemoveImage removes docker image
func (c *DockerClient) RemoveImage(imageID string) error {
	c.log.Infof("| Remove image %.12s", imageID)

	opts := docker.RemoveImageOptions{
		Force:   true,
		NoPrune: false,
	}
	return c.client.RemoveImageExtended(imageID, opts)
}

// CreateContainer creates docker container
func (c *DockerClient) CreateContainer(s State) (string, error) {

	s.Config.Image = s.ImageID

	// TODO: assign human readable name?

	opts := docker.CreateContainerOptions{
		Config:     &s.Config,
		HostConfig: &s.NoCache.HostConfig,
	}

	c.log.Debugf("Create container: %# v", pretty.Formatter(opts))

	container, err := c.client.CreateContainer(opts)
	if err != nil {
		return "", err
	}

	imageStr := fmt.Sprintf("(image %.12s)", s.ImageID)
	if s.ImageID == "" {
		imageStr = "(from scratch)"
	}

	c.log.Infof("| Created container %.12s %s", container.ID, imageStr)

	return container.ID, nil
}

// RunContainer runs docker container and optionally attaches stdin
func (c *DockerClient) RunContainer(containerID string, attachStdin bool) error {

	var (
		success   = make(chan struct{})
		finished  = make(chan struct{}, 1)
		sigch     = make(chan os.Signal, 1)
		errch     = make(chan error, 1)
		attacherr = make(chan error, 1)

		// Wrap output streams with logger
		outLogger = &logrus.Logger{
			Out:       c.log.Out,
			Formatter: NewContainerFormatter(containerID, logrus.InfoLevel),
			Level:     c.log.Level,
		}
		errLogger = &logrus.Logger{
			Out:       c.log.Out,
			Formatter: NewContainerFormatter(containerID, logrus.ErrorLevel),
			Level:     c.log.Level,
		}

		in                 = os.Stdin
		fdIn, isTerminalIn = term.GetFdInfo(in)
	)

	attachOpts := docker.AttachToContainerOptions{
		Container:    containerID,
		OutputStream: textformatter.LogWriter(outLogger),
		ErrorStream:  textformatter.LogWriter(errLogger),
		Stdout:       true,
		Stderr:       true,
		Stream:       true,
		Success:      success,
	}

	// Used by ATTACH
	if attachStdin {
		c.log.Infof("| Attach stdin to the container %.12s", containerID)

		if !isTerminalIn {
			return fmt.Errorf("Cannot attach to a container on non tty input")
		}

		attachOpts.InputStream = readerVoidCloser{in}
		attachOpts.OutputStream = os.Stdout
		attachOpts.ErrorStream = os.Stderr
		attachOpts.Stdin = true
		attachOpts.RawTerminal = true
	}

	// We want do debug the final attach options before setting raw term
	c.log.Debugf("Attach to container with options: %# v", attachOpts)

	if attachStdin {
		oldState, err := term.SetRawTerminal(fdIn)
		if err != nil {
			return err
		}
		defer term.RestoreTerminal(fdIn, oldState)
	}

	go func() {
		if err := c.client.AttachToContainer(attachOpts); err != nil {
			select {
			// Ignore any attach errors when we have finished already.
			// It may happen if we attach stdin, then container exit, but then there is other input from stdin continues.
			// This is the case when multiple ATTACH command are used in a single Rockerfile.
			// The problem though is that we cannot close stdin, to have it available for the subsequent ATTACH;
			// therefore, hijack goroutine from the previous ATTACH will hang until the input received and then
			// it will fire an error.
			// It's ok for `rocker` since it is not a daemon, but rather a one-off command.
			//
			// Also, there is still a problem that `rocker` loses second character from the Stdin in a second ATTACH.
			// But let's consider it a corner case.
			case <-finished:
				return
			default:
				attacherr <- fmt.Errorf("Got error while attaching to container %.12s: %s", containerID, err)
			}
		}
	}()

	success <- <-success

	// TODO: support options for container resources constraints like `docker build` has

	if err := c.client.StartContainer(containerID, &docker.HostConfig{}); err != nil {
		return err
	}

	if attachStdin {
		if err := c.monitorTtySize(containerID, os.Stdout); err != nil {
			return fmt.Errorf("Failed to monitor TTY size for container %.12s, error: %s", containerID, err)
		}
	}

	// TODO: move signal handling to the builder?

	signal.Notify(sigch, os.Interrupt)

	go func() {
		statusCode, err := c.client.WaitContainer(containerID)
		// c.log.Debugf("Wait finished, status %q error %q", statusCode, err)
		if err != nil {
			errch <- err
		} else if statusCode != 0 {
			errch <- fmt.Errorf("Container %.12s exited with code %d", containerID, statusCode)
		}
		errch <- nil
		return
	}()

	select {
	case err := <-errch:
		// indicate 'finished' so the `attach` goroutine will not give any errors
		finished <- struct{}{}
		if err != nil {
			return err
		}
	case err := <-attacherr:
		if err != nil {
			return err
		}
	case <-sigch:
		// TODO: Removing container twice for some reason
		c.log.Infof("Received SIGINT, remove current container...")
		if err := c.RemoveContainer(containerID); err != nil {
			c.log.Errorf("Failed to remove container: %s", err)
		}
		// TODO: send signal to builder.Run() and have a proper cleanup
		os.Exit(2)
	}

	return nil
}

// CommitContainer commits docker container
func (c *DockerClient) CommitContainer(s State, message string) (*docker.Image, error) {
	commitOpts := docker.CommitContainerOptions{
		Container: s.NoCache.ContainerID,
		Message:   message,
		Run:       &s.Config,
	}

	c.log.Debugf("Commit container: %# v", pretty.Formatter(commitOpts))

	image, err := c.client.CommitContainer(commitOpts)
	if err != nil {
		return nil, err
	}

	// Inspect the image to get the real size
	c.log.Debugf("Inspect image %s", image.ID)

	if image, err = c.client.InspectImage(image.ID); err != nil {
		return nil, err
	}

	size := fmt.Sprintf("%s (+%s)",
		units.HumanSize(float64(image.VirtualSize)),
		units.HumanSize(float64(image.Size)),
	)

	c.log.WithFields(logrus.Fields{
		"size": size,
	}).Infof("| Result image is %.12s", image.ID)

	return image, nil
}

// RemoveContainer removes docker container
func (c *DockerClient) RemoveContainer(containerID string) error {
	c.log.Infof("| Removing container %.12s", containerID)

	opts := docker.RemoveContainerOptions{
		ID:            containerID,
		Force:         true,
		RemoveVolumes: true,
	}

	return c.client.RemoveContainer(opts)
}

// UploadToContainer uploads files to a docker container
func (c *DockerClient) UploadToContainer(containerID string, stream io.Reader, path string) error {
	c.log.Infof("| Uploading files to container %.12s", containerID)

	opts := docker.UploadToContainerOptions{
		InputStream:          stream,
		Path:                 path,
		NoOverwriteDirNonDir: false,
	}

	return c.client.UploadToContainer(containerID, opts)
}

// TagImage adds tag to the image
func (c *DockerClient) TagImage(imageID, imageName string) error {
	img := imagename.NewFromString(imageName)

	c.log.Infof("| Tag %.12s -> %s", imageID, img)

	opts := docker.TagImageOptions{
		Repo:  img.NameWithRegistry(),
		Tag:   img.GetTag(),
		Force: true,
	}

	c.log.Debugf("Tag image %s with options: %# v", imageID, opts)

	return c.client.TagImage(imageID, opts)
}

// PushImage pushes the image
func (c *DockerClient) PushImage(imageName string) (digest string, err error) {
	var (
		img = imagename.NewFromString(imageName)

		buf                    bytes.Buffer
		pipeReader, pipeWriter = io.Pipe()
		outStream              = io.MultiWriter(pipeWriter, &buf)
		fdOut, isTerminalOut   = term.GetFdInfo(c.log.Out)
		out                    = c.log.Out

		opts = docker.PushImageOptions{
			Name:          img.NameWithRegistry(),
			Tag:           img.GetTag(),
			Registry:      img.Registry,
			OutputStream:  outStream,
			RawJSONStream: true,
		}
		errch = make(chan error, 1)
	)

	if !isTerminalOut {
		out = c.log.Writer()
	}

	c.log.Infof("| Push %s", img)

	c.log.Debugf("Push with options: %# v", opts)

	// TODO: DisplayJSONMessagesStream may fail by client.PushImage run without errors
	go func() {
		errch <- jsonmessage.DisplayJSONMessagesStream(pipeReader, out, fdOut, isTerminalOut)
	}()

	if err := c.client.PushImage(opts, c.auth); err != nil {
		return "", err
	}
	pipeWriter.Close()

	if err := <-errch; err != nil {
		return "", fmt.Errorf("Failed to process json stream, error %s", err)
	}

	// It is the best way to have pushed image digest so far
	matches := captureDigest.FindStringSubmatch(buf.String())
	if len(matches) > 0 {
		digest = matches[1]
	}

	return digest, nil
}

// ResolveHostPath proxy for the dockerclient.ResolveHostPath
func (c *DockerClient) ResolveHostPath(path string) (resultPath string, err error) {
	return dockerclient.ResolveHostPath(path, c.client)
}

// EnsureImage checks if the image exists and pulls if not
func (c *DockerClient) EnsureImage(imageName string) (err error) {

	var img *docker.Image
	if img, err = c.client.InspectImage(imageName); err != nil && err != docker.ErrNoSuchImage {
		return err
	}
	if img != nil {
		return nil
	}

	return c.PullImage(imageName)
}

// EnsureContainer checks if container with specified name exists
// and creates it otherwise
func (c *DockerClient) EnsureContainer(containerName string, config *docker.Config, purpose string) (containerID string, err error) {

	// Check if container exists
	container, err := c.client.InspectContainer(containerName)

	if _, ok := err.(*docker.NoSuchContainer); !ok && err != nil {
		return "", err
	}
	if container != nil {
		return container.ID, nil
	}

	// No data volume container for this build, create it

	if err := c.EnsureImage(config.Image); err != nil {
		return "", fmt.Errorf("Failed to check image %s, error: %s", config.Image, err)
	}

	c.log.Infof("| Create container: %s for %s", containerName, purpose)

	opts := docker.CreateContainerOptions{
		Name:   containerName,
		Config: config,
	}

	c.log.Debugf("Create container options %# v", opts)

	container, err = c.client.CreateContainer(opts)
	if err != nil {
		return "", fmt.Errorf("Failed to create container %s from image %s, error: %s", containerName, config.Image, err)
	}

	return container.ID, err
}

// InspectContainer simply inspects the container by name or ID
func (c *DockerClient) InspectContainer(containerName string) (container *docker.Container, err error) {
	return c.client.InspectContainer(containerName)
}
