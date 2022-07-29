/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// The kubelet binary is responsible for maintaining a set of containers on a particular host VM.
// It syncs data from both configuration file(s) as well as from a quorum of etcd servers.
// It then communicates with the container runtime (or a CRI shim for the runtime) to see what is
// currently running.  It synchronizes the configuration data, with the running set of containers
// by starting or stopping containers.
package main

import (
	"math/rand"
	"net"
	"time"

	"k8s.io/component-base/logs"
	_ "k8s.io/component-base/metrics/prometheus/restclient"
	_ "k8s.io/component-base/metrics/prometheus/version" // for version metric registration
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/cri/streaming"
	"k8s.io/kubernetes/pkg/kubelet/dockershim"
	dockerremote "k8s.io/kubernetes/pkg/kubelet/dockershim/remote"
)

const (
	// When these values are updated, also update test/utils/image/manifest.go
	defaultPodSandboxImageName    = "k8s.gcr.io/pause"
	defaultPodSandboxImageVersion = "3.4.1"
)

var (
	defaultPodSandboxImage = defaultPodSandboxImageName +
		":" + defaultPodSandboxImageVersion
)

func main() {
	rand.Seed(time.Now().UnixNano())

	logs.InitLogs()
	defer logs.FlushLogs()

	pluginSettings := dockershim.NetworkPluginSettings{
		HairpinMode:        kubeletconfiginternal.HairpinMode("promiscuous-bridge"),
		NonMasqueradeCIDR:  "10.0.0.0/8",
		PluginName:         "",
		PluginConfDir:      "/etc/cni/net.d",
		PluginBinDirString: "/opt/cni/bin",
		PluginCacheDir:     "/var/lib/cni/cache",
		MTU:                0,
	}

	remoteRuntimeEndpoint := "/run/runc/runc.sock"

	kubeDeps := &kubelet.Dependencies{
		DockerOptions: &kubelet.DockerOptions{
			DockerEndpoint:        "unix:///var/run/docker.sock",
			RuntimeRequestTimeout: 2 * time.Minute,
		},
	}

	// Create and start the CRI shim running as a grpc server.
	streamingConfig := getStreamingConfig()
	dockerClientConfig := &dockershim.ClientConfig{
		DockerEndpoint:            kubeDeps.DockerOptions.DockerEndpoint,
		RuntimeRequestTimeout:     kubeDeps.DockerOptions.RuntimeRequestTimeout,
		ImagePullProgressDeadline: kubeDeps.DockerOptions.ImagePullProgressDeadline,
	}

	ds, err := dockershim.NewDockerService(dockerClientConfig, defaultPodSandboxImage, streamingConfig,
		&pluginSettings, "", "cgroupfs", "/var/lib/dockershim")
	if err != nil {
		panic(err)
	}

	// The unix socket for kubelet <-> dockershim communication, dockershim start before runtime service init.
	klog.V(5).InfoS("Using remote runtime endpoint and image endpoint", "runtimeEndpoint", remoteRuntimeEndpoint)
	klog.V(2).InfoS("Starting the GRPC server for the docker CRI shim.")

	dockerServer := dockerremote.NewDockerServer(remoteRuntimeEndpoint, ds)
	if err := dockerServer.Start(); err != nil {
		panic(err)
	}

	dockerServer.Start()
	a := make(chan int)
	<-a
}

func getStreamingConfig() *streaming.Config {
	config := &streaming.Config{
		StreamIdleTimeout:               4 * time.Hour,
		StreamCreationTimeout:           streaming.DefaultConfig.StreamCreationTimeout,
		SupportedRemoteCommandProtocols: streaming.DefaultConfig.SupportedRemoteCommandProtocols,
		SupportedPortForwardProtocols:   streaming.DefaultConfig.SupportedPortForwardProtocols,
	}
	config.Addr = net.JoinHostPort("localhost", "0")
	return config
}
