/*
Copyright 2019 The Kubernetes Authors.

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

package nodeup

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"k8s.io/kops/nodeup/pkg/distros"
	"k8s.io/kops/nodeup/pkg/model"
	"k8s.io/kops/nodeup/pkg/model/networking"
	api "k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/apis/kops/registry"
	"k8s.io/kops/pkg/apis/nodeup"
	"k8s.io/kops/pkg/assets"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/nodeup/cloudinit"
	"k8s.io/kops/upup/pkg/fi/nodeup/local"
	"k8s.io/kops/upup/pkg/fi/nodeup/nodetasks"
	"k8s.io/kops/upup/pkg/fi/secrets"
	"k8s.io/kops/upup/pkg/fi/utils"
	"k8s.io/kops/util/pkg/architectures"
	"k8s.io/kops/util/pkg/vfs"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog"
)

// MaxTaskDuration is the amount of time to keep trying for; we retry for a long time - there is not really any great fallback
const MaxTaskDuration = 365 * 24 * time.Hour

// NodeUpCommand is the configuration for nodeup
type NodeUpCommand struct {
	CacheDir       string
	ConfigLocation string
	FSRoot         string
	Target         string
	cluster        *api.Cluster
	config         *nodeup.Config
	instanceGroup  *api.InstanceGroup
}

// Run is responsible for perform the nodeup process
func (c *NodeUpCommand) Run(out io.Writer) error {
	if c.FSRoot == "" {
		return fmt.Errorf("FSRoot is required")
	}

	if c.ConfigLocation != "" {
		config, err := vfs.Context.ReadFile(c.ConfigLocation)
		if err != nil {
			return fmt.Errorf("error loading configuration %q: %v", c.ConfigLocation, err)
		}

		err = utils.YamlUnmarshal(config, &c.config)
		if err != nil {
			return fmt.Errorf("error parsing configuration %q: %v", c.ConfigLocation, err)
		}
	} else {
		return fmt.Errorf("ConfigLocation is required")
	}

	if c.CacheDir == "" {
		return fmt.Errorf("CacheDir is required")
	}

	var configBase vfs.Path
	if fi.StringValue(c.config.ConfigBase) != "" {
		var err error
		configBase, err = vfs.Context.BuildVfsPath(*c.config.ConfigBase)
		if err != nil {
			return fmt.Errorf("cannot parse ConfigBase %q: %v", *c.config.ConfigBase, err)
		}
	} else if fi.StringValue(c.config.ClusterLocation) != "" {
		basePath := *c.config.ClusterLocation
		lastSlash := strings.LastIndex(basePath, "/")
		if lastSlash != -1 {
			basePath = basePath[0:lastSlash]
		}

		var err error
		configBase, err = vfs.Context.BuildVfsPath(basePath)
		if err != nil {
			return fmt.Errorf("cannot parse inferred ConfigBase %q: %v", basePath, err)
		}
	} else {
		return fmt.Errorf("ConfigBase is required")
	}

	c.cluster = &api.Cluster{}
	{
		clusterLocation := fi.StringValue(c.config.ClusterLocation)

		var p vfs.Path
		if clusterLocation != "" {
			var err error
			p, err = vfs.Context.BuildVfsPath(clusterLocation)
			if err != nil {
				return fmt.Errorf("error parsing ClusterLocation %q: %v", clusterLocation, err)
			}
		} else {
			p = configBase.Join(registry.PathClusterCompleted)
		}

		b, err := p.ReadFile()
		if err != nil {
			return fmt.Errorf("error loading Cluster %q: %v", p, err)
		}

		err = utils.YamlUnmarshal(b, c.cluster)
		if err != nil {
			return fmt.Errorf("error parsing Cluster %q: %v", p, err)
		}
	}

	if c.config.InstanceGroupName != "" {
		instanceGroupLocation := configBase.Join("instancegroup", c.config.InstanceGroupName)

		c.instanceGroup = &api.InstanceGroup{}
		b, err := instanceGroupLocation.ReadFile()
		if err != nil {
			return fmt.Errorf("error loading InstanceGroup %q: %v", instanceGroupLocation, err)
		}

		if err = utils.YamlUnmarshal(b, c.instanceGroup); err != nil {
			return fmt.Errorf("error parsing InstanceGroup %q: %v", instanceGroupLocation, err)
		}
	} else {
		klog.Warningf("No instance group defined in nodeup config")
	}

	err := evaluateSpec(c)
	if err != nil {
		return err
	}

	architecture, err := architectures.FindArchitecture()
	if err != nil {
		return fmt.Errorf("error determining OS architecture: %v", err)
	}

	archTags := architecture.BuildTags()

	distribution, err := distros.FindDistribution(c.FSRoot)
	if err != nil {
		return fmt.Errorf("error determining OS distribution: %v", err)
	}

	distroTags := distribution.BuildTags()

	nodeTags := sets.NewString()
	nodeTags.Insert(c.config.Tags...)
	nodeTags.Insert(archTags...)
	nodeTags.Insert(distroTags...)

	klog.Infof("Config tags: %v", c.config.Tags)
	klog.Infof("Arch tags: %v", archTags)
	klog.Infof("Distro tags: %v", distroTags)

	configAssets := c.config.Assets[architecture]
	assetStore := fi.NewAssetStore(c.CacheDir)
	for _, asset := range configAssets {
		err := assetStore.Add(asset)
		if err != nil {
			return fmt.Errorf("error adding asset %q: %v", asset, err)
		}
	}

	modelContext := &model.NodeupModelContext{
		Architecture:  architecture,
		Assets:        assetStore,
		Cluster:       c.cluster,
		ConfigBase:    configBase,
		Distribution:  distribution,
		InstanceGroup: c.instanceGroup,
		NodeupConfig:  c.config,
	}

	var secretStore fi.SecretStore
	var keyStore fi.Keystore
	if c.cluster.Spec.SecretStore != "" {
		klog.Infof("Building SecretStore at %q", c.cluster.Spec.SecretStore)
		p, err := vfs.Context.BuildVfsPath(c.cluster.Spec.SecretStore)
		if err != nil {
			return fmt.Errorf("error building secret store path: %v", err)
		}

		secretStore = secrets.NewVFSSecretStore(c.cluster, p)
		modelContext.SecretStore = secretStore
	} else {
		return fmt.Errorf("SecretStore not set")
	}

	if c.cluster.Spec.KeyStore != "" {
		klog.Infof("Building KeyStore at %q", c.cluster.Spec.KeyStore)
		p, err := vfs.Context.BuildVfsPath(c.cluster.Spec.KeyStore)
		if err != nil {
			return fmt.Errorf("error building key store path: %v", err)
		}

		modelContext.KeyStore = fi.NewVFSCAStore(c.cluster, p)
		keyStore = modelContext.KeyStore
	} else {
		return fmt.Errorf("KeyStore not set")
	}

	if err := modelContext.Init(); err != nil {
		return err
	}

	if err := loadKernelModules(modelContext); err != nil {
		return err
	}

	loader := &Loader{}
	loader.Builders = append(loader.Builders, &model.NTPBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.MiscUtilsBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.DirectoryBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.UpdateServiceBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.VolumesBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.ContainerdBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.DockerBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.ProtokubeBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.CloudConfigBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.FileAssetsBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.HookBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.NodeAuthorizationBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.KubeletBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.KubectlBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.EtcdBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.LogrotateBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.ManifestsBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.PackagesBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.SecretBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.FirewallBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.SysctlBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.KubeAPIServerBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.KubeControllerManagerBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.KubeSchedulerBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.EtcdManagerTLSBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &model.KubeProxyBuilder{NodeupModelContext: modelContext})

	loader.Builders = append(loader.Builders, &networking.CommonBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &networking.CalicoBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &networking.CanalBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &networking.CiliumBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &networking.FlannelBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &networking.KuberouterBuilder{NodeupModelContext: modelContext})
	loader.Builders = append(loader.Builders, &networking.LyftVPCBuilder{NodeupModelContext: modelContext})

	taskMap, err := loader.Build()
	if err != nil {
		return fmt.Errorf("error building loader: %v", err)
	}

	for i, image := range c.config.Images[architecture] {
		taskMap["LoadImage."+strconv.Itoa(i)] = &nodetasks.LoadImageTask{
			Sources: image.Sources,
			Hash:    image.Hash,
			Runtime: c.cluster.Spec.ContainerRuntime,
		}
	}
	// Protokube load image task is in ProtokubeBuilder

	var cloud fi.Cloud
	var target fi.Target
	checkExisting := true

	switch c.Target {
	case "direct":
		target = &local.LocalTarget{
			CacheDir: c.CacheDir,
			Tags:     nodeTags,
		}
	case "dryrun":
		assetBuilder := assets.NewAssetBuilder(c.cluster, "")
		target = fi.NewDryRunTarget(assetBuilder, out)
	case "cloudinit":
		checkExisting = false
		target = cloudinit.NewCloudInitTarget(out, nodeTags)
	default:
		return fmt.Errorf("unsupported target type %q", c.Target)
	}

	context, err := fi.NewContext(target, nil, cloud, keyStore, secretStore, configBase, checkExisting, taskMap)
	if err != nil {
		klog.Exitf("error building context: %v", err)
	}
	defer context.Close()

	var options fi.RunTasksOptions
	options.InitDefaults()

	err = context.RunTasks(options)
	if err != nil {
		klog.Exitf("error running tasks: %v", err)
	}

	err = target.Finish(taskMap)
	if err != nil {
		klog.Exitf("error closing target: %v", err)
	}

	return nil
}

func evaluateSpec(c *NodeUpCommand) error {
	var err error

	c.cluster.Spec.Kubelet.HostnameOverride, err = evaluateHostnameOverride(c.cluster.Spec.Kubelet.HostnameOverride)
	if err != nil {
		return err
	}

	c.cluster.Spec.MasterKubelet.HostnameOverride, err = evaluateHostnameOverride(c.cluster.Spec.MasterKubelet.HostnameOverride)
	if err != nil {
		return err
	}

	c.config.KubeletConfig.HostnameOverride, err = evaluateHostnameOverride(c.config.KubeletConfig.HostnameOverride)
	if err != nil {
		return err
	}

	if c.cluster.Spec.KubeProxy != nil {
		c.cluster.Spec.KubeProxy.HostnameOverride, err = evaluateHostnameOverride(c.cluster.Spec.KubeProxy.HostnameOverride)
		if err != nil {
			return err
		}
		c.cluster.Spec.KubeProxy.BindAddress, err = evaluateBindAddress(c.cluster.Spec.KubeProxy.BindAddress)
		if err != nil {
			return err
		}
	}

	if c.cluster.Spec.Docker != nil {
		err = evaluateDockerSpecStorage(c.cluster.Spec.Docker)
		if err != nil {
			return err
		}
	}

	return nil
}

func evaluateHostnameOverride(hostnameOverride string) (string, error) {
	if hostnameOverride == "" || hostnameOverride == "@hostname" {
		return "", nil
	}
	k := strings.TrimSpace(hostnameOverride)
	k = strings.ToLower(k)

	if k == "@aws" {
		// We recognize @aws as meaning "the private DNS name from AWS", to generate this we need to get a few pieces of information
		azBytes, err := vfs.Context.ReadFile("metadata://aws/meta-data/placement/availability-zone")
		if err != nil {
			return "", fmt.Errorf("error reading availability zone from AWS metadata: %v", err)
		}

		instanceIDBytes, err := vfs.Context.ReadFile("metadata://aws/meta-data/instance-id")
		if err != nil {
			return "", fmt.Errorf("error reading instance-id from AWS metadata: %v", err)
		}
		instanceID := string(instanceIDBytes)

		config := aws.NewConfig()
		config = config.WithCredentialsChainVerboseErrors(true)

		s, err := session.NewSession(config)
		if err != nil {
			return "", fmt.Errorf("error starting new AWS session: %v", err)
		}

		svc := ec2.New(s, config.WithRegion(string(azBytes[:len(azBytes)-1])))

		result, err := svc.DescribeInstances(&ec2.DescribeInstancesInput{
			InstanceIds: []*string{&instanceID},
		})
		if err != nil {
			return "", fmt.Errorf("error describing instances: %v", err)
		}

		if len(result.Reservations) != 1 {
			return "", fmt.Errorf("Too many reservations returned for the single instance-id")
		}

		if len(result.Reservations[0].Instances) != 1 {
			return "", fmt.Errorf("Too many instances returned for the single instance-id")
		}
		return *(result.Reservations[0].Instances[0].PrivateDnsName), nil
	}

	if k == "@gce" {
		// We recognize @gce as meaning the hostname from the GCE metadata service
		// This lets us tolerate broken hostnames (i.e. systemd)
		b, err := vfs.Context.ReadFile("metadata://gce/instance/hostname")
		if err != nil {
			return "", fmt.Errorf("error reading hostname from GCE metadata: %v", err)
		}

		// We only want to use the first portion of the fully-qualified name
		// e.g. foo.c.project.internal => foo
		fullyQualified := string(b)
		bareHostname := strings.Split(fullyQualified, ".")[0]
		return bareHostname, nil
	}

	if k == "@digitalocean" {
		// @digitalocean means to use the private ipv4 address of a droplet as the hostname override
		vBytes, err := vfs.Context.ReadFile("metadata://digitalocean/interfaces/private/0/ipv4/address")
		if err != nil {
			return "", fmt.Errorf("error reading droplet private IP from DigitalOcean metadata: %v", err)
		}

		hostname := string(vBytes)
		if hostname == "" {
			return "", errors.New("private IP for digitalocean droplet was empty")
		}

		return hostname, nil
	}

	if k == "@alicloud" {
		// @alicloud means to use the "{az}.{instance-id}" of a instance as the hostname override
		azBytes, err := vfs.Context.ReadFile("metadata://alicloud/zone-id")
		if err != nil {
			return "", fmt.Errorf("error reading zone-id from Alicloud metadata: %v", err)
		}
		az := string(azBytes)

		instanceIDBytes, err := vfs.Context.ReadFile("metadata://alicloud/instance-id")
		if err != nil {
			return "", fmt.Errorf("error reading instance-id from Alicloud metadata: %v", err)
		}
		instanceID := string(instanceIDBytes)

		return fmt.Sprintf("%s.%s", az, instanceID), nil
	}

	return hostnameOverride, nil
}

func evaluateBindAddress(bindAddress string) (string, error) {
	if bindAddress == "" {
		return "", nil
	}
	if bindAddress == "@aws" {
		vBytes, err := vfs.Context.ReadFile("metadata://aws/meta-data/local-ipv4")
		if err != nil {
			return "", fmt.Errorf("error reading local IP from AWS metadata: %v", err)
		}

		// The local-ipv4 gets it's IP from the AWS.
		// For now just choose the first one.
		ips := strings.Fields(string(vBytes))
		if len(ips) == 0 {
			klog.Warningf("Local IP from AWS metadata service was empty")
			return "", nil
		}

		ip := ips[0]
		klog.Infof("Using IP from AWS metadata service: %s", ip)

		return ip, nil
	}

	if net.ParseIP(bindAddress) == nil {
		return "", fmt.Errorf("bindAddress is not valid IP address")
	}
	return bindAddress, nil
}

// evaluateDockerSpec selects the first supported storage mode, if it is a list
func evaluateDockerSpecStorage(spec *api.DockerConfig) error {
	storage := fi.StringValue(spec.Storage)
	if strings.Contains(fi.StringValue(spec.Storage), ",") {
		precedence := strings.Split(storage, ",")
		for _, opt := range precedence {
			fs := opt
			if fs == "overlay2" {
				fs = "overlay"
			}
			supported, err := kernelHasFilesystem(fs)
			if err != nil {
				klog.Warningf("error checking if %q filesystem is supported: %v", fs, err)
				continue
			}

			if !supported {
				// overlay -> overlay
				// aufs -> aufs
				module := fs
				if err = modprobe(fs); err != nil {
					klog.Warningf("error running `modprobe %q`: %v", module, err)
				}
			}

			supported, err = kernelHasFilesystem(fs)
			if err != nil {
				klog.Warningf("error checking if %q filesystem is supported: %v", fs, err)
				continue
			}

			if supported {
				klog.Infof("Using supported docker storage %q", opt)
				spec.Storage = fi.String(opt)
				return nil
			}

			klog.Warningf("%q docker storage was specified, but filesystem is not supported", opt)
		}

		// Just in case we don't recognize the driver?
		// TODO: Is this the best behaviour
		klog.Warningf("No storage module was supported from %q, will default to %q", storage, precedence[0])
		spec.Storage = fi.String(precedence[0])
		return nil
	}

	return nil
}

// kernelHasFilesystem checks if /proc/filesystems contains the specified filesystem
func kernelHasFilesystem(fs string) (bool, error) {
	contents, err := ioutil.ReadFile("/proc/filesystems")
	if err != nil {
		return false, fmt.Errorf("error reading /proc/filesystems: %v", err)
	}

	for _, line := range strings.Split(string(contents), "\n") {
		tokens := strings.Fields(line)
		for _, token := range tokens {
			// Technically we should skip "nodev", but it doesn't matter
			if token == fs {
				return true, nil
			}
		}
	}

	return false, nil
}

// modprobe will exec `modprobe <module>`
func modprobe(module string) error {
	klog.Infof("Doing modprobe for module %v", module)
	out, err := exec.Command("/sbin/modprobe", module).CombinedOutput()
	outString := string(out)
	if err != nil {
		return fmt.Errorf("modprobe for module %q failed (%v): %s", module, err, outString)
	}
	if outString != "" {
		klog.Infof("Output from modprobe %s:\n%s", module, outString)
	}
	return nil
}

// loadKernelModules is a hack to force br_netfilter to be loaded
// TODO: Move to tasks architecture
func loadKernelModules(context *model.NodeupModelContext) error {
	err := modprobe("br_netfilter")
	if err != nil {
		// TODO: Return error in 1.11 (too risky for 1.10)
		klog.Warningf("error loading br_netfilter module: %v", err)
	}
	// TODO: Add to /etc/modules-load.d/ ?
	return nil
}
