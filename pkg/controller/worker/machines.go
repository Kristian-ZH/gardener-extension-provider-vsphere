/*
 * Copyright 2019 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 *
 */

package worker

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"

	apisvsphere "github.com/gardener/gardener-extension-provider-vsphere/pkg/apis/vsphere"
	"github.com/gardener/gardener-extension-provider-vsphere/pkg/apis/vsphere/helper"
	"github.com/gardener/gardener-extension-provider-vsphere/pkg/vsphere"

	extensionscontroller "github.com/gardener/gardener/extensions/pkg/controller"
	"github.com/gardener/gardener/extensions/pkg/controller/worker"
	corev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	machinev1alpha1 "github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"
)

// MachineClassKind yields the name of the vSphere machine class.
func (w *workerDelegate) MachineClassKind() string {
	return "MachineClass"
}

// MachineClassList yields a newly initialized VsphereMachineClassList object.
func (w *workerDelegate) MachineClassList() runtime.Object {
	return &machinev1alpha1.MachineClassList{}
}

// DeployMachineClasses generates and creates the vSphere specific machine classes.
func (w *workerDelegate) DeployMachineClasses(ctx context.Context) error {
	if w.machineClasses == nil {
		if err := w.generateMachineConfig(ctx); err != nil {
			return err
		}
	}
	return w.seedChartApplier.Apply(ctx, filepath.Join(vsphere.InternalChartsPath, "machineclass"), w.worker.Namespace, "machineclass", kubernetes.Values(map[string]interface{}{"machineClasses": w.machineClasses}))
}

// GenerateMachineDeployments generates the configuration for the desired machine deployments.
func (w *workerDelegate) GenerateMachineDeployments(ctx context.Context) (worker.MachineDeployments, error) {
	if w.machineDeployments == nil {
		if err := w.generateMachineConfig(ctx); err != nil {
			return nil, err
		}
	}
	return w.machineDeployments, nil
}

func (w *workerDelegate) generateMachineClassSecretData(ctx context.Context) (map[string][]byte, error) {
	secret, err := extensionscontroller.GetSecretByReference(ctx, w.Client(), &w.worker.Spec.SecretRef)
	if err != nil {
		return nil, err
	}

	credentials, err := vsphere.ExtractCredentials(secret)
	if err != nil {
		return nil, err
	}

	region := helper.FindRegion(w.cluster.Shoot.Spec.Region, w.cloudProfileConfig)
	if region == nil {
		return nil, fmt.Errorf("region %q not found", w.cluster.Shoot.Spec.Region)
	}

	return map[string][]byte{
		vsphere.Host:        []byte(region.VsphereHost),
		vsphere.Username:    []byte(credentials.VsphereMCM().Username),
		vsphere.Password:    []byte(credentials.VsphereMCM().Password),
		vsphere.InsecureSSL: []byte(strconv.FormatBool(region.VsphereInsecureSSL)),
	}, nil
}

func (w *workerDelegate) generateMachineConfig(ctx context.Context) error {
	var (
		machineDeployments = worker.MachineDeployments{}
		machineClasses     []map[string]interface{}
		machineImages      []apisvsphere.MachineImage
	)

	machineClassSecretData, err := w.generateMachineClassSecretData(ctx)
	if err != nil {
		return err
	}

	infrastructureStatus, err := helper.GetInfrastructureStatus(w.worker.Namespace, w.worker.Spec.InfrastructureProviderStatus)
	if err != nil {
		return err
	}
	if infrastructureStatus.NSXTInfraState == nil || infrastructureStatus.NSXTInfraState.SegmentName == nil {
		return fmt.Errorf("SegmentName not set in nsxtInfraState")
	}

	if len(w.worker.Spec.SSHPublicKey) == 0 {
		return fmt.Errorf("missing sshPublicKey for infrastructure")
	}

	for _, pool := range w.worker.Spec.Pools {
		zoneLen := int32(len(pool.Zones))

		workerPoolHash, err := worker.WorkerPoolHash(pool, w.cluster)
		if err != nil {
			return err
		}

		machineImagePath, machineImageGuestID, err := w.findMachineImage(pool.MachineImage.Name, pool.MachineImage.Version)
		if err != nil {
			return err
		}
		machineImages = appendMachineImage(machineImages, apisvsphere.MachineImage{
			Name:    pool.MachineImage.Name,
			Version: pool.MachineImage.Version,
			Path:    machineImagePath,
			GuestID: machineImageGuestID,
		})

		numCpus, memoryInMB, systenDiskSizeInGB, err := w.extractMachineValues(pool.MachineType)
		if err != nil {
			return errors.Wrap(err, "extracting machine values failed")
		}

		for zoneIndex, zone := range pool.Zones {
			zoneIdx := int32(zoneIndex)
			zoneConfig, ok := infrastructureStatus.VsphereConfig.ZoneConfigs[zone]
			if !ok {
				return fmt.Errorf("zoneConfig not found for zone %s", zone)
			}
			machineClassSpec := map[string]interface{}{
				"region":     infrastructureStatus.VsphereConfig.Region,
				"sshKeys":    []string{string(w.worker.Spec.SSHPublicKey)},
				"datacenter": zoneConfig.Datacenter,
				"network":    *infrastructureStatus.NSXTInfraState.SegmentName,
				"templateVM": machineImagePath,
				"numCpus":    numCpus,
				"memory":     memoryInMB,
				"systemDisk": map[string]interface{}{
					"size": systenDiskSizeInGB,
				},
				"tags": map[string]string{
					"mcm.gardener.cloud/cluster": w.worker.Namespace,
					"mcm.gardener.cloud/role":    "node",
				},
				"secret": map[string]interface{}{
					"cloudConfig": string(pool.UserData),
				},
			}
			addOptional := func(key, value string) {
				if value != "" {
					machineClassSpec[key] = value
				}
			}
			addOptional("folder", infrastructureStatus.VsphereConfig.Folder)
			addOptional("guestId", machineImageGuestID)
			addOptional("hostSystem", zoneConfig.HostSystem)
			addOptional("resourcePool", zoneConfig.ResourcePool)
			addOptional("computeCluster", zoneConfig.ComputeCluster)
			addOptional("datastore", zoneConfig.Datastore)
			addOptional("datastoreCluster", zoneConfig.DatastoreCluster)
			addOptional("switchUuid", zoneConfig.SwitchUUID)

			var (
				deploymentName = fmt.Sprintf("%s-%s-z%d", w.worker.Namespace, pool.Name, zoneIndex+1)
				className      = fmt.Sprintf("%s-%s", deploymentName, workerPoolHash)
			)

			machineDeployments = append(machineDeployments, worker.MachineDeployment{
				Name:           deploymentName,
				ClassName:      className,
				SecretName:     className,
				Minimum:        worker.DistributeOverZones(zoneIdx, pool.Minimum, zoneLen),
				Maximum:        worker.DistributeOverZones(zoneIdx, pool.Maximum, zoneLen),
				MaxSurge:       worker.DistributePositiveIntOrPercent(zoneIdx, pool.MaxSurge, zoneLen, pool.Maximum),
				MaxUnavailable: worker.DistributePositiveIntOrPercent(zoneIdx, pool.MaxUnavailable, zoneLen, pool.Minimum),
				Labels:         pool.Labels,
				Annotations:    pool.Annotations,
				Taints:         pool.Taints,
			})

			machineClassSpec["name"] = className
			secretMap := machineClassSpec["secret"].(map[string]interface{})
			for k, v := range machineClassSecretData {
				secretMap[k] = string(v)
			}

			machineClasses = append(machineClasses, machineClassSpec)
		}
	}

	w.machineDeployments = machineDeployments
	w.machineClasses = machineClasses
	w.machineImages = machineImages

	return nil
}

func (w *workerDelegate) extractMachineValues(machineTypeName string) (numCpus, memoryInMB, systemDiskSizeInGB int, err error) {
	var machineType *corev1beta1.MachineType
	for _, mt := range w.cluster.CloudProfile.Spec.MachineTypes {
		if mt.Name == machineTypeName {
			machineType = &mt
			break
		}
	}
	if machineType == nil {
		err = fmt.Errorf("machine type %s not found in cloud profile spec", machineTypeName)
		return
	}

	if n, ok := machineType.CPU.AsInt64(); ok {
		numCpus = int(n)
	}
	if numCpus <= 0 {
		err = fmt.Errorf("machine type %s has invalid CPU value %s", machineTypeName, machineType.CPU.String())
		return
	}

	if n, ok := machineType.Memory.AsInt64(); ok {
		memoryInMB = int(n) / (1024 * 1024)
	}
	if memoryInMB <= 0 {
		err = fmt.Errorf("machine type %s has invalid Memory value %s", machineTypeName, machineType.CPU.String())
		return
	}

	systemDiskSizeInGB = 20
	if machineType.Storage != nil {
		n, ok := machineType.Storage.StorageSize.AsInt64()
		if !ok {
			err = fmt.Errorf("machine type %s has invalid storage size value %s", machineTypeName, machineType.Storage.StorageSize.String())
			return
		}
		systemDiskSizeInGB = int(n) / (1024 * 1024 * 1024)
		if systemDiskSizeInGB < 10 {
			err = fmt.Errorf("machine type %s has invalid storage size value %d GB", machineTypeName, systemDiskSizeInGB)
			return
		}
	}

	return
}
