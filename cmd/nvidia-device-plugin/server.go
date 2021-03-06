/*
 * Copyright (c) 2019, NVIDIA CORPORATION.  All rights reserved.
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

package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/NVIDIA/go-gpuallocator/gpuallocator"
	config "github.com/NVIDIA/k8s-device-plugin/api/config/v1"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// Constants to represent the various device list strategies
const (
	DeviceListStrategyEnvvar       = "envvar"
	DeviceListStrategyVolumeMounts = "volume-mounts"
)

// Constants to represent the various device id strategies
const (
	DeviceIDStrategyUUID  = "uuid"
	DeviceIDStrategyIndex = "index"
)

// Constants for use by the 'volume-mounts' device list strategy
const (
	deviceListAsVolumeMountsHostPath          = "/dev/null"
	deviceListAsVolumeMountsContainerPathRoot = "/var/run/nvidia-container-devices"
)

// NvidiaDevicePlugin implements the Kubernetes device plugin API
type NvidiaDevicePlugin struct {
	ResourceManager
	config           config.Config
	resourceName     string
	deviceListEnvvar string
	allocatePolicy   gpuallocator.Policy
	socket           string
	replicas         uint
	autoReplicas     bool

	server         *grpc.Server
	cachedDevices  []*Device // raw devices
	deviceReplicas []*Device // devices presented to k8s that include the replicas
	health         chan *Device
	stop           chan interface{}
}

// NewNvidiaDevicePlugin returns an initialized NvidiaDevicePlugin
func NewNvidiaDevicePlugin(config *config.Config, resourceName string, resourceManager ResourceManager, deviceListEnvvar string, allocatePolicy gpuallocator.Policy, socket string, replicas uint, autoReplicas bool) *NvidiaDevicePlugin {
	return &NvidiaDevicePlugin{
		ResourceManager:  resourceManager,
		config:           *config,
		resourceName:     resourceName,
		deviceListEnvvar: deviceListEnvvar,
		allocatePolicy:   allocatePolicy,
		socket:           socket,
		replicas:         replicas,
		autoReplicas:     autoReplicas,

		// These will be reinitialized every
		// time the plugin server is restarted.
		cachedDevices:  nil,
		deviceReplicas: nil,
		server:         nil,
		health:         nil,
		stop:           nil,
	}
}

func (m *NvidiaDevicePlugin) initialize() {
	m.cachedDevices = m.Devices()

	for _, dev := range m.cachedDevices {
		replicas := m.replicas
		if m.autoReplicas {
			// Dividing the total memory to avoid reaching a limit of about 64K devices
			replicas = uint(dev.TotalMemory / 1000)
		}

		log.Printf("Replicating device %v %v times", *dev, replicas)
		for i := uint(0); i < replicas; i++ {
			replicatedDev := *dev // This is replicating the Device struct
			replicatedDev.ID = fmt.Sprintf("%s%s%d", dev.ID, joinStr, i)
			m.deviceReplicas = append(m.deviceReplicas, &replicatedDev)
		}
	}

	m.server = grpc.NewServer([]grpc.ServerOption{}...)
	m.health = make(chan *Device)
	m.stop = make(chan interface{})
}

func (m *NvidiaDevicePlugin) cleanup() {
	close(m.stop)
	m.cachedDevices = nil
	m.deviceReplicas = nil
	m.server = nil
	m.health = nil
	m.stop = nil
}

// Start starts the gRPC server, registers the device plugin with the Kubelet,
// and starts the device healthchecks.
func (m *NvidiaDevicePlugin) Start() error {
	m.initialize()

	err := m.Serve()
	if err != nil {
		log.Printf("Could not start device plugin for '%s': %s", m.resourceName, err)
		m.cleanup()
		return err
	}
	log.Printf("Starting to serve '%s' on %s", m.resourceName, m.socket)

	err = m.Register()
	if err != nil {
		log.Printf("Could not register device plugin: %s", err)
		m.Stop()
		return err
	}
	log.Printf("Registered device plugin for '%s' with Kubelet", m.resourceName)

	go m.CheckHealth(m.stop, m.cachedDevices, m.health)

	return nil
}

// Stop stops the gRPC server.
func (m *NvidiaDevicePlugin) Stop() error {
	if m == nil || m.server == nil {
		return nil
	}
	log.Printf("Stopping to serve '%s' on %s", m.resourceName, m.socket)
	m.server.Stop()
	if err := os.Remove(m.socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	m.cleanup()
	return nil
}

// Serve starts the gRPC server of the device plugin.
func (m *NvidiaDevicePlugin) Serve() error {
	os.Remove(m.socket)
	sock, err := net.Listen("unix", m.socket)
	if err != nil {
		return err
	}

	pluginapi.RegisterDevicePluginServer(m.server, m)

	go func() {
		lastCrashTime := time.Now()
		restartCount := 0
		for {
			log.Printf("Starting GRPC server for '%s'", m.resourceName)
			err := m.server.Serve(sock)
			if err == nil {
				break
			}

			log.Printf("GRPC server for '%s' crashed with error: %v", m.resourceName, err)

			// restart if it has not been too often
			// i.e. if server has crashed more than 5 times and it didn't last more than one hour each time
			if restartCount > 5 {
				// quit
				log.Fatalf("GRPC server for '%s' has repeatedly crashed recently. Quitting", m.resourceName)
			}
			timeSinceLastCrash := time.Since(lastCrashTime).Seconds()
			lastCrashTime = time.Now()
			if timeSinceLastCrash > 3600 {
				// it has been one hour since the last crash.. reset the count
				// to reflect on the frequency
				restartCount = 1
			} else {
				restartCount++
			}
		}
	}()

	// Wait for server to start by launching a blocking connexion
	conn, err := m.dial(m.socket, 5*time.Second)
	if err != nil {
		return err
	}
	conn.Close()

	return nil
}

// Register registers the device plugin for the given resourceName with Kubelet.
func (m *NvidiaDevicePlugin) Register() error {
	conn, err := m.dial(pluginapi.KubeletSocket, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)
	reqt := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(m.socket),
		ResourceName: m.resourceName,
		Options: &pluginapi.DevicePluginOptions{
			GetPreferredAllocationAvailable: (m.allocatePolicy != nil || m.replicas > 1 || m.autoReplicas),
		},
	}

	_, err = client.Register(context.Background(), reqt)
	if err != nil {
		return err
	}
	return nil
}

// GetDevicePluginOptions returns the values of the optional settings for this plugin
func (m *NvidiaDevicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	options := &pluginapi.DevicePluginOptions{
		GetPreferredAllocationAvailable: (m.allocatePolicy != nil || m.replicas > 1 || m.autoReplicas),
	}
	return options, nil
}

// ListAndWatch lists devices and update that list according to the health status
func (m *NvidiaDevicePlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	s.Send(&pluginapi.ListAndWatchResponse{Devices: m.apiDevices()})

	for {
		select {
		case <-m.stop:
			return nil
		case d := <-m.health:
			// FIXME: there is no way to recover from the Unhealthy state.
			d.Health = pluginapi.Unhealthy
			log.Printf("'%s' device marked unhealthy: %s", m.resourceName, d.ID)
			s.Send(&pluginapi.ListAndWatchResponse{Devices: m.apiDevices()})
		}
	}
}

// GetPreferredAllocation returns the preferred allocation from the set of devices specified in the request
func (m *NvidiaDevicePlugin) GetPreferredAllocation(ctx context.Context, r *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	// Note there should only be -replica-0 and not any -replica-1 or -replica-2, etc.
	// since this function is only called when we have no replicas.

	response := &pluginapi.PreferredAllocationResponse{}
	for _, req := range r.ContainerRequests {
		available, err := gpuallocator.NewDevicesFrom(stripReplicas(req.AvailableDeviceIDs))
		if err != nil {
			return nil, fmt.Errorf("unable to retrieve list of available devices: %v", err)
		}

		required, err := gpuallocator.NewDevicesFrom(stripReplicas(req.MustIncludeDeviceIDs))
		if err != nil {
			return nil, fmt.Errorf("unable to retrieve list of required devices: %v", err)
		}

		var deviceIds []string
		if m.replicas > 1 || m.autoReplicas {
			ids, err := prioritizeDevices(req.AvailableDeviceIDs, req.MustIncludeDeviceIDs, int(req.AllocationSize))
			if err != nil {
				var nonUnique *NonUniqueError
				if errors.As(err, &nonUnique) {
					// non unique assignment is not fatal however sub-optimal
					log.Print("Ignoring: ", nonUnique)
				} else {
					return nil, err
				}
			}
			deviceIds = ids
		} else if m.allocatePolicy != nil {
			allocated := m.allocatePolicy.Allocate(available, required, int(req.AllocationSize))
			for _, device := range allocated {
				deviceIds = append(deviceIds, device.UUID)
			}
		} else {
			return nil, errors.New("GetPreferredAllocation() not implemented in this case")
		}

		resp := &pluginapi.ContainerPreferredAllocationResponse{
			DeviceIDs: deviceIds,
		}

		response.ContainerResponses = append(response.ContainerResponses, resp)
	}
	return response, nil
}

// Allocate which return list of devices.
func (m *NvidiaDevicePlugin) Allocate(ctx context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	responses := pluginapi.AllocateResponse{}
	for _, req := range reqs.ContainerRequests {
		for _, id := range req.DevicesIDs {
			if !m.deviceReplicaExists(id) {
				return nil, fmt.Errorf("invalid allocation request for '%s': unknown device: %s", m.resourceName, id)
			}
		}

		uuids := stripReplicas(req.DevicesIDs)
		log.Printf("kubelet is requesting devices %s, but using raw devices %s", req.DevicesIDs, uuids)

		for _, id := range uuids {
			if !m.deviceExists(id) {
				return nil, fmt.Errorf("invalid allocation request for '%s': unknown device: %s", m.resourceName, id)
			}
		}

		response := pluginapi.ContainerAllocateResponse{}

		deviceIDs := m.deviceIDsFromUUIDs(uuids)

		if m.config.Flags.DeviceListStrategy == DeviceListStrategyEnvvar {
			response.Envs = m.apiEnvs(m.deviceListEnvvar, deviceIDs)
		}
		if m.config.Flags.DeviceListStrategy == DeviceListStrategyVolumeMounts {
			response.Envs = m.apiEnvs(m.deviceListEnvvar, []string{deviceListAsVolumeMountsContainerPathRoot})
			response.Mounts = m.apiMounts(deviceIDs)
		}
		if m.config.Flags.PassDeviceSpecs {
			response.Devices = m.apiDeviceSpecs(m.config.Flags.NvidiaDriverRoot, uuids)
		}

		responses.ContainerResponses = append(responses.ContainerResponses, &response)
	}

	return &responses, nil
}

// PreStartContainer is unimplemented for this plugin
func (m *NvidiaDevicePlugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

// dial establishes the gRPC communication with the registered device plugin.
func (m *NvidiaDevicePlugin) dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	c, err := grpc.Dial(unixSocketPath, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithTimeout(timeout),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}),
	)

	if err != nil {
		return nil, err
	}

	return c, nil
}

// deviceExists checks if a k8s device exists
func (m *NvidiaDevicePlugin) deviceExists(id string) bool {
	for _, d := range m.cachedDevices {
		if d.ID == id {
			return true
		}
	}
	return false
}

// deviceReplicaExists checks if a k8s device replica exists
func (m *NvidiaDevicePlugin) deviceReplicaExists(id string) bool {
	for _, d := range m.deviceReplicas {
		if d.ID == id {
			return true
		}
	}
	return false
}

// apiDevices returns the K8S API Device type. This includes replicas
func (m *NvidiaDevicePlugin) deviceIDsFromUUIDs(uuids []string) []string {
	if m.config.Flags.DeviceIDStrategy == DeviceIDStrategyUUID {
		return uuids
	}

	var deviceIDs []string
	if m.config.Flags.DeviceIDStrategy == DeviceIDStrategyIndex {
		for _, d := range m.cachedDevices {
			for _, id := range uuids {
				if d.ID == id {
					deviceIDs = append(deviceIDs, d.Index)
				}
			}
		}
	}
	return deviceIDs
}

func (m *NvidiaDevicePlugin) apiDevices() []*pluginapi.Device {
	var pdevs []*pluginapi.Device
	for _, d := range m.deviceReplicas {
		pdevs = append(pdevs, &d.Device)
	}
	return pdevs
}

func (m *NvidiaDevicePlugin) apiEnvs(envvar string, deviceIDs []string) map[string]string {
	return map[string]string{
		envvar: strings.Join(deviceIDs, ","),
	}
}

func (m *NvidiaDevicePlugin) apiMounts(deviceIDs []string) []*pluginapi.Mount {
	var mounts []*pluginapi.Mount

	for _, id := range deviceIDs {
		mount := &pluginapi.Mount{
			HostPath:      deviceListAsVolumeMountsHostPath,
			ContainerPath: filepath.Join(deviceListAsVolumeMountsContainerPathRoot, id),
		}
		mounts = append(mounts, mount)
	}

	return mounts
}

func (m *NvidiaDevicePlugin) apiDeviceSpecs(driverRoot string, uuids []string) []*pluginapi.DeviceSpec {
	var specs []*pluginapi.DeviceSpec

	paths := []string{
		"/dev/nvidiactl",
		"/dev/nvidia-uvm",
		"/dev/nvidia-uvm-tools",
		"/dev/nvidia-modeset",
	}

	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			spec := &pluginapi.DeviceSpec{
				ContainerPath: p,
				HostPath:      filepath.Join(driverRoot, p),
				Permissions:   "rw",
			}
			specs = append(specs, spec)
		}
	}

	for _, d := range m.cachedDevices {
		for _, id := range uuids {
			if d.ID == id {
				for _, p := range d.Paths {
					spec := &pluginapi.DeviceSpec{
						ContainerPath: p,
						HostPath:      filepath.Join(driverRoot, p),
						Permissions:   "rw",
					}
					specs = append(specs, spec)
				}
			}
		}
	}

	return specs
}
