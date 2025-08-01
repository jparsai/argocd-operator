// Copyright 2019 ArgoCD Operator Developers
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package argocd

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	argoproj "github.com/argoproj-labs/argocd-operator/api/v1beta1"
	"github.com/argoproj-labs/argocd-operator/common"
	"github.com/argoproj-labs/argocd-operator/controllers/argoutil"
)

func getRedisHAReplicas() *int32 {
	replicas := common.ArgoCDDefaultRedisHAReplicas
	// TODO: Allow override of this value through CR?
	return &replicas
}

// newStatefulSet returns a new StatefulSet instance for the given ArgoCD instance.
func newStatefulSet(cr *argoproj.ArgoCD) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name,
			Namespace: cr.Namespace,
			Labels:    argoutil.LabelsForCluster(cr),
		},
	}
}

// newStatefulSetWithName returns a new StatefulSet instance for the given ArgoCD using the given name.
func newStatefulSetWithName(name string, component string, cr *argoproj.ArgoCD) *appsv1.StatefulSet {
	ss := newStatefulSet(cr)
	ss.ObjectMeta.Name = name

	lbls := ss.ObjectMeta.Labels
	lbls[common.ArgoCDKeyName] = name
	lbls[common.ArgoCDKeyComponent] = component
	ss.ObjectMeta.Labels = lbls

	ss.Spec = appsv1.StatefulSetSpec{
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				common.ArgoCDKeyName: name,
			},
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					common.ArgoCDKeyName: name,
				},
				Annotations: make(map[string]string),
			},
			Spec: corev1.PodSpec{
				NodeSelector: common.DefaultNodeSelector(),
			},
		},
	}
	if cr.Spec.NodePlacement != nil {
		ss.Spec.Template.Spec.NodeSelector = argoutil.AppendStringMap(ss.Spec.Template.Spec.NodeSelector, cr.Spec.NodePlacement.NodeSelector)
		ss.Spec.Template.Spec.Tolerations = cr.Spec.NodePlacement.Tolerations
	}
	ss.Spec.ServiceName = name

	return ss
}

// newStatefulSetWithSuffix returns a new StatefulSet instance for the given ArgoCD using the given suffix.
func newStatefulSetWithSuffix(suffix string, component string, cr *argoproj.ArgoCD) *appsv1.StatefulSet {
	return newStatefulSetWithName(fmt.Sprintf("%s-%s", cr.Name, suffix), component, cr)
}

func (r *ReconcileArgoCD) reconcileRedisStatefulSet(cr *argoproj.ArgoCD) error {
	ss := newStatefulSetWithSuffix("redis-ha-server", "redis", cr)

	redisEnv := append(proxyEnvVars(), corev1.EnvVar{
		Name: "AUTH",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: fmt.Sprintf("%s-%s", cr.Name, "redis-initial-password"),
				},
				Key: "admin.password",
			},
		},
	})

	ss.Spec.PodManagementPolicy = appsv1.OrderedReadyPodManagement
	ss.Spec.Replicas = getRedisHAReplicas()
	ss.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			common.ArgoCDKeyName: nameWithSuffix("redis-ha", cr),
		},
	}

	ss.Spec.ServiceName = nameWithSuffix("redis-ha", cr)

	ss.Spec.Template.ObjectMeta = metav1.ObjectMeta{
		Annotations: map[string]string{
			"checksum/init-config": "7128bfbb51eafaffe3c33b1b463e15f0cf6514cec570f9d9c4f2396f28c724ac", // TODO: Should this be hard-coded?
		},
		Labels: map[string]string{
			common.ArgoCDKeyName: nameWithSuffix("redis-ha", cr),
		},
	}

	ss.Spec.Template.Spec.Affinity = &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						common.ArgoCDKeyName: nameWithSuffix("redis-ha", cr),
					},
				},
				TopologyKey: common.ArgoCDKeyHostname,
			}},
		},
	}

	f := false
	ss.Spec.Template.Spec.AutomountServiceAccountToken = &f

	ss.Spec.Template.Spec.Containers = []corev1.Container{
		{
			Args: []string{
				"/data/conf/redis.conf",
			},
			Command: []string{
				"redis-server",
			},
			Env:             redisEnv,
			Image:           getRedisHAContainerImage(cr),
			ImagePullPolicy: corev1.PullIfNotPresent,
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"/health/redis_liveness.sh",
						},
					},
				},
				FailureThreshold:    int32(5),
				InitialDelaySeconds: int32(30),
				PeriodSeconds:       int32(15),
				SuccessThreshold:    int32(1),
				TimeoutSeconds:      int32(15),
			},
			Name: "redis",
			Ports: []corev1.ContainerPort{{
				ContainerPort: common.ArgoCDDefaultRedisPort,
				Name:          "redis",
			}},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"/health/redis_readiness.sh",
						},
					},
				},
				FailureThreshold:    int32(5),
				InitialDelaySeconds: int32(30),
				PeriodSeconds:       int32(15),
				SuccessThreshold:    int32(1),
				TimeoutSeconds:      int32(15),
			},
			Resources: getRedisHAResources(cr),
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: boolPtr(false),
				Capabilities: &corev1.Capabilities{
					Drop: []corev1.Capability{
						"ALL",
					},
				},
				ReadOnlyRootFilesystem: boolPtr(true),
				RunAsNonRoot:           boolPtr(true),
				SeccompProfile: &corev1.SeccompProfile{
					Type: "RuntimeDefault",
				},
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					MountPath: "/data",
					Name:      "data",
				},
				{
					MountPath: "/health",
					Name:      "health",
				},
				{
					Name:      common.ArgoCDRedisServerTLSSecretName,
					MountPath: "/app/config/redis/tls",
				},
			},
		},
		{
			Args: []string{
				"/data/conf/sentinel.conf",
			},
			Command: []string{
				"redis-sentinel",
			},
			Env:             redisEnv,
			Image:           getRedisHAContainerImage(cr),
			ImagePullPolicy: corev1.PullIfNotPresent,
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"/health/sentinel_liveness.sh",
						},
					},
				},
				FailureThreshold:    int32(5),
				InitialDelaySeconds: int32(30),
				PeriodSeconds:       int32(15),
				SuccessThreshold:    int32(1),
				TimeoutSeconds:      int32(15),
			},
			Name: "sentinel",
			Ports: []corev1.ContainerPort{{
				ContainerPort: common.ArgoCDDefaultRedisSentinelPort,
				Name:          "sentinel",
			}},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							"/health/sentinel_liveness.sh",
						},
					},
				},
				FailureThreshold:    int32(5),
				InitialDelaySeconds: int32(30),
				PeriodSeconds:       int32(15),
				SuccessThreshold:    int32(1),
				TimeoutSeconds:      int32(15),
			},
			Resources: getRedisHAResources(cr),
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: boolPtr(false),
				Capabilities: &corev1.Capabilities{
					Drop: []corev1.Capability{
						"ALL",
					},
				},
				ReadOnlyRootFilesystem: boolPtr(true),
				RunAsNonRoot:           boolPtr(true),
				SeccompProfile: &corev1.SeccompProfile{
					Type: "RuntimeDefault",
				},
			},
			Lifecycle: &corev1.Lifecycle{
				PostStart: &corev1.LifecycleHandler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"/bin/sh",
							"-c",
							func() string {
								// Check if TLS is enabled for Redis
								useTLS := r.redisShouldUseTLS(cr)
								if useTLS {
									// Use TLS for redis-cli when connecting to sentinel
									return "sleep 30; redis-cli -p 26379 --tls --cert /app/config/redis/tls/tls.crt --key /app/config/redis/tls/tls.key --insecure sentinel reset argocd"
								}
								return "sleep 30; redis-cli -p 26379 sentinel reset argocd"
							}(),
						},
					},
				},
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					MountPath: "/data",
					Name:      "data",
				},
				{
					MountPath: "/health",
					Name:      "health",
				},
				{
					Name:      common.ArgoCDRedisServerTLSSecretName,
					MountPath: "/app/config/redis/tls",
				},
			},
		},
	}

	ss.Spec.Template.Spec.InitContainers = []corev1.Container{{
		Args: []string{
			"/readonly-config/init.sh",
		},
		Command: []string{
			"sh",
		},
		Env: []corev1.EnvVar{
			{
				Name:  "SENTINEL_ID_0",
				Value: "3c0d9c0320bb34888c2df5757c718ce6ca992ce6", // TODO: Should this be hard-coded?
			},
			{
				Name:  "SENTINEL_ID_1",
				Value: "40000915ab58c3fa8fd888fb8b24711944e6cbb4", // TODO: Should this be hard-coded?
			},
			{
				Name:  "SENTINEL_ID_2",
				Value: "2bbec7894d954a8af3bb54d13eaec53cb024e2ca", // TODO: Should this be hard-coded?
			},
			{
				Name: "AUTH",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: fmt.Sprintf("%s-%s", cr.Name, "redis-initial-password"),
						},
						Key: "admin.password",
					},
				},
			},
		},
		Image:           getRedisHAContainerImage(cr),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Name:            "config-init",
		Resources:       getRedisHAResources(cr),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{
					"ALL",
				},
			},
			ReadOnlyRootFilesystem: boolPtr(true),
			RunAsNonRoot:           boolPtr(true),
			SeccompProfile: &corev1.SeccompProfile{
				Type: "RuntimeDefault",
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				MountPath: "/readonly-config",
				Name:      "config",
				ReadOnly:  true,
			},
			{
				MountPath: "/data",
				Name:      "data",
			},
			{
				Name:      common.ArgoCDRedisServerTLSSecretName,
				MountPath: "/app/config/redis/tls",
			},
		},
	}}

	if IsOpenShiftCluster() {
		var runAsNonRoot bool = true
		ss.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{
			RunAsNonRoot: &runAsNonRoot,
		}
	} else {
		var fsGroup int64 = 1000
		var runAsNonRoot bool = true
		var runAsUser int64 = 1000

		ss.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{
			FSGroup:      &fsGroup,
			RunAsNonRoot: &runAsNonRoot,
			RunAsUser:    &runAsUser,
		}
	}
	AddSeccompProfileForOpenShift(r.Client, &ss.Spec.Template.Spec)

	ss.Spec.Template.Spec.ServiceAccountName = nameWithSuffix("argocd-redis-ha", cr)

	var terminationGracePeriodSeconds int64 = 60
	ss.Spec.Template.Spec.TerminationGracePeriodSeconds = &terminationGracePeriodSeconds

	var defaultMode int32 = 493
	ss.Spec.Template.Spec.Volumes = []corev1.Volume{
		{
			Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: common.ArgoCDRedisHAConfigMapName,
					},
				},
			},
		},
		{
			Name: "health",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					DefaultMode: &defaultMode,
					LocalObjectReference: corev1.LocalObjectReference{
						Name: common.ArgoCDRedisHAHealthConfigMapName,
					},
				},
			},
		},
		{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: common.ArgoCDRedisServerTLSSecretName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: common.ArgoCDRedisServerTLSSecretName,
					Optional:   boolPtr(true),
				},
			},
		},
	}

	ss.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
		Type: appsv1.RollingUpdateStatefulSetStrategyType,
	}

	if err := applyReconcilerHook(cr, ss, ""); err != nil {
		return err
	}

	existing := newStatefulSetWithSuffix("redis-ha-server", "redis", cr)
	ssExists, err := argoutil.IsObjectFound(r.Client, cr.Namespace, existing.Name, existing)
	if err != nil {
		return err
	}
	if ssExists {
		if !(cr.Spec.HA.Enabled && cr.Spec.Redis.IsEnabled()) {
			// StatefulSet exists but either HA or component enabled flag has been set to false, delete the StatefulSet
			var explanation string
			if !cr.Spec.HA.Enabled {
				explanation = "ha is disabled"
			} else {
				explanation = "redis is disabled"
			}
			argoutil.LogResourceDeletion(log, existing, explanation)
			return r.Client.Delete(context.TODO(), existing)
		}

		desiredImage := getRedisHAContainerImage(cr)
		changed := false
		explanation := ""
		updateNodePlacementStateful(existing, ss, &changed, &explanation)
		for i, container := range existing.Spec.Template.Spec.Containers {
			if container.Image != desiredImage {
				existing.Spec.Template.Spec.Containers[i].Image = getRedisHAContainerImage(cr)
				existing.Spec.Template.ObjectMeta.Labels["image.upgraded"] = time.Now().UTC().Format("01022006-150406-MST")
				if changed {
					explanation += ", "
				}
				explanation += fmt.Sprintf("container '%s' image", container.Name)
				changed = true
			}
			if !reflect.DeepEqual(ss.Spec.Template.Spec.Containers[i].VolumeMounts, existing.Spec.Template.Spec.Containers[i].VolumeMounts) {
				existing.Spec.Template.Spec.Containers[i].VolumeMounts = ss.Spec.Template.Spec.Containers[i].VolumeMounts
				if changed {
					explanation += ", "
				}
				explanation += fmt.Sprintf("container '%s' VolumeMounts", container.Name)
				changed = true
			}

			if !reflect.DeepEqual(ss.Spec.Template.Spec.Containers[i].Resources, existing.Spec.Template.Spec.Containers[i].Resources) {
				existing.Spec.Template.Spec.Containers[i].Resources = ss.Spec.Template.Spec.Containers[i].Resources
				if changed {
					explanation += ", "
				}
				explanation += fmt.Sprintf("container '%s' resources", container.Name)
				changed = true
			}

			if !reflect.DeepEqual(ss.Spec.Template.Spec.Containers[i].SecurityContext, existing.Spec.Template.Spec.Containers[i].SecurityContext) {
				existing.Spec.Template.Spec.Containers[i].SecurityContext = ss.Spec.Template.Spec.Containers[i].SecurityContext
				if changed {
					explanation += ", "
				}
				explanation += fmt.Sprintf("container '%s' security context", container.Name)
				changed = true
			}

			if !reflect.DeepEqual(ss.Spec.Template.Spec.Containers[i].Env, existing.Spec.Template.Spec.Containers[i].Env) {
				existing.Spec.Template.Spec.Containers[i].Env = ss.Spec.Template.Spec.Containers[i].Env
				if changed {
					explanation += ", "
				}
				explanation += fmt.Sprintf("container '%s' env", container.Name)
				changed = true
			}
		}
		if !reflect.DeepEqual(ss.Spec.Template.Spec.SecurityContext, existing.Spec.Template.Spec.SecurityContext) {
			existing.Spec.Template.Spec.SecurityContext = ss.Spec.Template.Spec.SecurityContext
			if changed {
				explanation += ", "
			}
			explanation += "security context"
			changed = true
		}
		if !reflect.DeepEqual(ss.Spec.Template.Spec.Volumes, existing.Spec.Template.Spec.Volumes) {
			existing.Spec.Template.Spec.Volumes = ss.Spec.Template.Spec.Volumes
			if changed {
				explanation += ", "
			}
			explanation += "volumes"
			changed = true
		}
		if !reflect.DeepEqual(ss.Spec.Template.Spec.InitContainers, existing.Spec.Template.Spec.InitContainers) {
			existing.Spec.Template.Spec.InitContainers = ss.Spec.Template.Spec.InitContainers
			if changed {
				explanation += ", "
			}
			explanation += "init containers"
			changed = true
		}
		if changed {
			argoutil.LogResourceUpdate(log, existing, "updating", explanation)
			return r.Client.Update(context.TODO(), existing)
		}

		return nil // StatefulSet found, do nothing
	}

	if cr.Spec.Redis.IsEnabled() && cr.Spec.Redis.Remote != nil && *cr.Spec.Redis.Remote != "" {
		log.Info("Custom Redis Endpoint. Skipping starting redis.")
		return nil
	}

	if !cr.Spec.Redis.IsEnabled() {
		log.Info("Redis disabled. Skipping starting Redis.") // Redis not enabled, do nothing.
		return nil
	}

	if !cr.Spec.HA.Enabled {
		return nil // HA not enabled, do nothing.
	}

	if err := controllerutil.SetControllerReference(cr, ss, r.Scheme); err != nil {
		return err
	}
	argoutil.LogResourceCreation(log, ss)
	return r.Client.Create(context.TODO(), ss)
}

func getArgoControllerContainerEnv(cr *argoproj.ArgoCD, replicas int32) []corev1.EnvVar {
	env := make([]corev1.EnvVar, 0)

	env = append(env, corev1.EnvVar{
		Name:  "HOME",
		Value: "/home/argocd",
	})

	env = append(env, corev1.EnvVar{
		Name: "REDIS_PASSWORD",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: fmt.Sprintf("%s-%s", cr.Name, "redis-initial-password"),
				},
				Key: "admin.password",
			},
		},
	})

	if cr.Spec.Controller.Sharding.Enabled || (cr.Spec.Controller.Sharding.DynamicScalingEnabled != nil && *cr.Spec.Controller.Sharding.DynamicScalingEnabled) {
		env = append(env, corev1.EnvVar{
			Name:  "ARGOCD_CONTROLLER_REPLICAS",
			Value: fmt.Sprint(replicas),
		})
	}

	if cr.Spec.Controller.AppSync != nil {
		env = append(env, corev1.EnvVar{
			Name:  "ARGOCD_RECONCILIATION_TIMEOUT",
			Value: strconv.FormatInt(int64(cr.Spec.Controller.AppSync.Seconds()), 10) + "s",
		})
	}

	env = append(env, corev1.EnvVar{
		Name: "ARGOCD_CONTROLLER_RESOURCE_HEALTH_PERSIST",
		ValueFrom: &corev1.EnvVarSource{
			ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: common.ArgoCDCmdParamsConfigMapName,
				},
				Key: "controller.resource.health.persist",
			},
		},
	},
	)
	return env
}

func (r *ReconcileArgoCD) getApplicationControllerReplicaCount(cr *argoproj.ArgoCD) int32 {
	var replicas int32 = common.ArgocdApplicationControllerDefaultReplicas
	var minShards int32 = cr.Spec.Controller.Sharding.MinShards
	var maxShards int32 = cr.Spec.Controller.Sharding.MaxShards

	if cr.Spec.Controller.Sharding.DynamicScalingEnabled != nil && *cr.Spec.Controller.Sharding.DynamicScalingEnabled {

		// TODO: add the same validations to Validation Webhook once webhook has been introduced
		if minShards < 1 {
			log.Info("Minimum number of shards cannot be less than 1. Setting default value to 1")
			minShards = 1
		}

		if maxShards < minShards {
			log.Info("Maximum number of shards cannot be less than minimum number of shards. Setting maximum shards same as minimum shards")
			maxShards = minShards
		}

		clustersPerShard := cr.Spec.Controller.Sharding.ClustersPerShard
		if clustersPerShard < 1 {
			log.Info("clustersPerShard cannot be less than 1. Defaulting to 1.")
			clustersPerShard = 1
		}

		clusterSecrets, err := r.getClusterSecrets(cr)
		if err != nil {
			// If we were not able to query cluster secrets, return the default count of replicas (ArgocdApplicationControllerDefaultReplicas)
			log.Error(err, "Error retreiving cluster secrets for ArgoCD instance %s", cr.Name)
			return replicas
		}

		replicas = int32(len(clusterSecrets.Items)) / clustersPerShard

		if replicas < minShards {
			replicas = minShards
		}

		if replicas > maxShards {
			replicas = maxShards
		}

		return replicas

	} else if cr.Spec.Controller.Sharding.Replicas != 0 && cr.Spec.Controller.Sharding.Enabled {
		return cr.Spec.Controller.Sharding.Replicas
	}

	return replicas
}

func (r *ReconcileArgoCD) reconcileApplicationControllerStatefulSet(cr *argoproj.ArgoCD, useTLSForRedis bool) error {

	replicas := r.getApplicationControllerReplicaCount(cr)

	ss := newStatefulSetWithSuffix("application-controller", "application-controller", cr)
	ss.Spec.Replicas = &replicas
	controllerEnv := cr.Spec.Controller.Env
	// Sharding setting explicitly overrides a value set in the env
	controllerEnv = argoutil.EnvMerge(controllerEnv, getArgoControllerContainerEnv(cr, replicas), true)
	// Let user specify their own environment first
	controllerEnv = argoutil.EnvMerge(controllerEnv, proxyEnvVars(), false)

	if cr.Spec.Controller.InitContainers != nil {
		ss.Spec.Template.Spec.InitContainers = append(ss.Spec.Template.Spec.InitContainers, cr.Spec.Controller.InitContainers...)
	}

	controllerVolumeMounts := []corev1.VolumeMount{
		{
			Name:      "argocd-repo-server-tls",
			MountPath: "/app/config/controller/tls",
		},
		{
			Name:      common.ArgoCDRedisServerTLSSecretName,
			MountPath: "/app/config/controller/tls/redis",
		},
		{
			Name:      "argocd-home",
			MountPath: "/home/argocd",
		},
		{
			Name:      "argocd-cmd-params-cm",
			MountPath: "/home/argocd/params",
		},
		{
			Name:      "argocd-application-controller-tmp",
			MountPath: "/tmp",
		},
	}

	if cr.Spec.Controller.VolumeMounts != nil {
		controllerVolumeMounts = append(controllerVolumeMounts, cr.Spec.Controller.VolumeMounts...)
	}

	podSpec := &ss.Spec.Template.Spec
	podSpec.Containers = []corev1.Container{{
		Command:         getArgoApplicationControllerCommand(cr, useTLSForRedis),
		Image:           getArgoContainerImage(cr),
		ImagePullPolicy: corev1.PullAlways,
		Name:            "argocd-application-controller",
		Env:             controllerEnv,
		Ports: []corev1.ContainerPort{
			{
				ContainerPort: 8082,
			},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromInt(8082),
				},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
		Resources: getArgoApplicationControllerResources(cr),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{
					"ALL",
				},
			},
			ReadOnlyRootFilesystem: boolPtr(true),
			RunAsNonRoot:           boolPtr(true),
			SeccompProfile: &corev1.SeccompProfile{
				Type: "RuntimeDefault",
			},
		},
		VolumeMounts: controllerVolumeMounts,
	}}

	if cr.Spec.Controller.SidecarContainers != nil {
		ss.Spec.Template.Spec.Containers = append(ss.Spec.Template.Spec.Containers, cr.Spec.Controller.SidecarContainers...)
	}

	AddSeccompProfileForOpenShift(r.Client, podSpec)
	podSpec.ServiceAccountName = nameWithSuffix("argocd-application-controller", cr)

	controllerVolumes := []corev1.Volume{
		{
			Name: "argocd-repo-server-tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: common.ArgoCDRepoServerTLSSecretName,
					Optional:   boolPtr(true),
				},
			},
		},
		{
			Name: common.ArgoCDRedisServerTLSSecretName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: common.ArgoCDRedisServerTLSSecretName,
					Optional:   boolPtr(true),
				},
			},
		},
		{
			Name: "argocd-home",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "argocd-cmd-params-cm",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "argocd-cmd-params-cm",
					},
					Optional: boolPtr(true),
					Items: []corev1.KeyToPath{
						{
							Key:  "controller.profile.enabled",
							Path: "profiler.enabled",
						},
						{
							Key:  "controller.resource.health.persist",
							Path: "controller.resource.health.persist",
						},
					},
				},
			},
		},
		{
			Name: "argocd-application-controller-tmp",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	if cr.Spec.Controller.Volumes != nil {
		controllerVolumes = append(controllerVolumes, cr.Spec.Controller.Volumes...)
	}

	podSpec.Volumes = controllerVolumes

	ss.Spec.Template.Spec.Affinity = &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
				PodAffinityTerm: corev1.PodAffinityTerm{
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							common.ArgoCDKeyName: nameWithSuffix("argocd-application-controller", cr),
						},
					},
					TopologyKey: common.ArgoCDKeyHostname,
				},
				Weight: int32(100),
			},
				{
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								common.ArgoCDKeyPartOf: common.ArgoCDAppName,
							},
						},
						TopologyKey: common.ArgoCDKeyHostname,
					},
					Weight: int32(5),
				}},
		},
	}

	// Handle import/restore from ArgoCDExport
	export, err := r.getArgoCDExport(cr)
	if err != nil {
		return err
	}
	if export == nil {
		log.Info("existing argocd export not found, skipping import")
	} else {

		containerCommand, err := getArgoImportCommand(r.Client, cr)
		if err != nil {
			return err
		}

		podSpec.InitContainers = []corev1.Container{{
			Command:         containerCommand,
			Env:             proxyEnvVars(getArgoImportContainerEnv(export)...),
			Resources:       getArgoApplicationControllerResources(cr),
			Image:           getArgoImportContainerImage(export),
			ImagePullPolicy: corev1.PullAlways,
			Name:            "argocd-import",
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: boolPtr(false),
				Capabilities: &corev1.Capabilities{
					Drop: []corev1.Capability{
						"ALL",
					},
				},
				ReadOnlyRootFilesystem: boolPtr(true),
				RunAsNonRoot:           boolPtr(true),
				SeccompProfile: &corev1.SeccompProfile{
					Type: "RuntimeDefault",
				},
			},
			VolumeMounts: getArgoImportVolumeMounts(),
		}}

		podSpec.Volumes = getArgoImportVolumes(export)
	}

	invalidImagePod, err := containsInvalidImage(*cr, *r)
	if err != nil {
		return err
	} else if invalidImagePod {
		argoutil.LogResourceDeletion(log, ss, "one or more pods has an invalid image")
		if err := r.Client.Delete(context.TODO(), ss); err != nil {
			return err
		}
	}

	if cr.Spec.Controller.Annotations != nil {
		for key, value := range cr.Spec.Controller.Annotations {
			ss.Spec.Template.Annotations[key] = value
		}
	}

	if cr.Spec.Controller.Labels != nil {
		for key, value := range cr.Spec.Controller.Labels {
			ss.Spec.Template.Labels[key] = value
		}
	}

	existing := newStatefulSetWithSuffix("application-controller", "application-controller", cr)
	ssExists, err := argoutil.IsObjectFound(r.Client, cr.Namespace, existing.Name, existing)
	if err != nil {
		return err
	}
	if ssExists {
		if !cr.Spec.Controller.IsEnabled() {
			// Delete existing deployment for Application Controller, if any ..
			argoutil.LogResourceDeletion(log, existing, "application controller is disabled")
			return r.Client.Delete(context.TODO(), existing)
		}
		actualImage := existing.Spec.Template.Spec.Containers[0].Image
		desiredImage := getArgoContainerImage(cr)
		changed := false
		explanation := ""
		if actualImage != desiredImage {
			existing.Spec.Template.Spec.Containers[0].Image = desiredImage
			existing.Spec.Template.ObjectMeta.Labels["image.upgraded"] = time.Now().UTC().Format("01022006-150406-MST")
			explanation = "container image"
			changed = true
		}
		desiredCommand := getArgoApplicationControllerCommand(cr, useTLSForRedis)
		if isRepoServerTLSVerificationRequested(cr) {
			desiredCommand = append(desiredCommand, "--repo-server-strict-tls")
		}
		updateNodePlacementStateful(existing, ss, &changed, &explanation)
		if !reflect.DeepEqual(desiredCommand, existing.Spec.Template.Spec.Containers[0].Command) {
			existing.Spec.Template.Spec.Containers[0].Command = desiredCommand
			if changed {
				explanation += ", "
			}
			explanation += "container command"
			changed = true
		}
		if !reflect.DeepEqual(existing.Spec.Template.Spec.InitContainers, ss.Spec.Template.Spec.InitContainers) {
			existing.Spec.Template.Spec.InitContainers = ss.Spec.Template.Spec.InitContainers
			if changed {
				explanation += ", "
			}
			explanation += "init containers"
			changed = true
		}
		if !reflect.DeepEqual(existing.Spec.Template.Spec.Containers[0].Env,
			ss.Spec.Template.Spec.Containers[0].Env) {
			existing.Spec.Template.Spec.Containers[0].Env = ss.Spec.Template.Spec.Containers[0].Env
			if changed {
				explanation += ", "
			}
			explanation += "container env"
			changed = true
		}
		if !reflect.DeepEqual(ss.Spec.Template.Spec.Volumes, existing.Spec.Template.Spec.Volumes) {
			existing.Spec.Template.Spec.Volumes = ss.Spec.Template.Spec.Volumes
			if changed {
				explanation += ", "
			}
			explanation += "volumes"
			changed = true
		}
		if !reflect.DeepEqual(ss.Spec.Template.Spec.Containers[0].VolumeMounts,
			existing.Spec.Template.Spec.Containers[0].VolumeMounts) {
			existing.Spec.Template.Spec.Containers[0].VolumeMounts = ss.Spec.Template.Spec.Containers[0].VolumeMounts
			if changed {
				explanation += ", "
			}
			explanation += "container volume mounts"
			changed = true
		}
		if !reflect.DeepEqual(ss.Spec.Template.Spec.Containers[0].Resources, existing.Spec.Template.Spec.Containers[0].Resources) {
			existing.Spec.Template.Spec.Containers[0].Resources = ss.Spec.Template.Spec.Containers[0].Resources
			if changed {
				explanation += ", "
			}
			explanation += "container resources"
			changed = true
		}
		if !reflect.DeepEqual(ss.Spec.Template.Spec.Containers[0].SecurityContext, existing.Spec.Template.Spec.Containers[0].SecurityContext) {
			existing.Spec.Template.Spec.Containers[0].SecurityContext = ss.Spec.Template.Spec.Containers[0].SecurityContext
			if changed {
				explanation += ", "
			}
			explanation += "container security context"
			changed = true
		}
		if !reflect.DeepEqual(ss.Spec.Replicas, existing.Spec.Replicas) {
			existing.Spec.Replicas = ss.Spec.Replicas
			if changed {
				explanation += ", "
			}
			explanation += "replicas"
			changed = true
		}
		if !reflect.DeepEqual(ss.Spec.Template.Spec.SecurityContext, existing.Spec.Template.Spec.SecurityContext) {
			existing.Spec.Template.Spec.SecurityContext = ss.Spec.Template.Spec.SecurityContext
			if changed {
				explanation += ", "
			}
			explanation += "security context"
			changed = true
		}

		if !reflect.DeepEqual(ss.Spec.Template.Spec.Containers[1:],
			existing.Spec.Template.Spec.Containers[1:]) {
			existing.Spec.Template.Spec.Containers = append(existing.Spec.Template.Spec.Containers[0:1],
				ss.Spec.Template.Spec.Containers[1:]...)
			if changed {
				explanation += ", "
			}
			explanation += "additional containers"
			changed = true
		}

		//Check if labels/annotations have changed
		UpdateMapValues(&existing.Spec.Template.Labels, ss.Spec.Template.Labels)
		UpdateMapValues(&existing.Spec.Template.Annotations, ss.Spec.Template.Annotations)

		if !reflect.DeepEqual(ss.Spec.Template.Annotations, existing.Spec.Template.Annotations) {
			existing.Spec.Template.Annotations = ss.Spec.Template.Annotations
			if changed {
				explanation += ", "
			}
			explanation += "annotations"
			changed = true
		}

		if !reflect.DeepEqual(ss.Spec.Template.Labels, existing.Spec.Template.Labels) {
			existing.Spec.Template.Labels = ss.Spec.Template.Labels
			if changed {
				explanation += ", "
			}
			explanation += "labels"
			changed = true
		}
		if changed {
			argoutil.LogResourceUpdate(log, existing, "updating", explanation)
			return r.Client.Update(context.TODO(), existing)
		}

		return nil // StatefulSet found with nothing to do, move along...
	}

	if !cr.Spec.Controller.IsEnabled() {
		log.Info("Application Controller disabled. Skipping starting application controller.")
		return nil
	}

	// Delete existing deployment for Application Controller, if any ..
	deploy := newDeploymentWithSuffix("application-controller", "application-controller", cr)
	deplExists, err := argoutil.IsObjectFound(r.Client, deploy.Namespace, deploy.Name, deploy)
	if err != nil {
		return err
	}
	if deplExists {
		argoutil.LogResourceDeletion(log, deploy, "application controller is configured using stateful set, not deployment")
		if err := r.Client.Delete(context.TODO(), deploy); err != nil {
			return err
		}
	}

	if err := controllerutil.SetControllerReference(cr, ss, r.Scheme); err != nil {
		return err
	}
	argoutil.LogResourceCreation(log, ss)
	return r.Client.Create(context.TODO(), ss)
}

// reconcileStatefulSets will ensure that all StatefulSets are present for the given ArgoCD.
func (r *ReconcileArgoCD) reconcileStatefulSets(cr *argoproj.ArgoCD, useTLSForRedis bool) error {
	if err := r.reconcileApplicationControllerStatefulSet(cr, useTLSForRedis); err != nil {
		return err
	}
	if err := r.reconcileRedisStatefulSet(cr); err != nil {
		return err
	}
	return nil
}

// triggerStatefulSetRollout will update the label with the given key to trigger a new rollout of the StatefulSet.
func (r *ReconcileArgoCD) triggerStatefulSetRollout(sts *appsv1.StatefulSet, key string) error {
	ssExists, err := argoutil.IsObjectFound(r.Client, sts.Namespace, sts.Name, sts)
	if err != nil {
		return err
	}
	if !ssExists {
		log.Info(fmt.Sprintf("unable to locate deployment with name: %s", sts.Name))
		return nil
	}

	sts.Spec.Template.ObjectMeta.Labels[key] = nowNano()
	argoutil.LogResourceUpdate(log, sts, "to trigger rollout")
	return r.Client.Update(context.TODO(), sts)
}

// to update nodeSelector and tolerations in reconciler
func updateNodePlacementStateful(existing *appsv1.StatefulSet, ss *appsv1.StatefulSet, changed *bool, explanation *string) {
	if !reflect.DeepEqual(existing.Spec.Template.Spec.NodeSelector, ss.Spec.Template.Spec.NodeSelector) {
		existing.Spec.Template.Spec.NodeSelector = ss.Spec.Template.Spec.NodeSelector
		if *changed {
			*explanation += ", "
		}
		*explanation += "node selector"
		*changed = true
	}
	if !reflect.DeepEqual(existing.Spec.Template.Spec.Tolerations, ss.Spec.Template.Spec.Tolerations) {
		existing.Spec.Template.Spec.Tolerations = ss.Spec.Template.Spec.Tolerations
		if *changed {
			*explanation += ", "
		}
		*explanation += "tolerations"
		*changed = true
	}
}

// Returns true if a StatefulSet has pods in ErrImagePull or ImagePullBackoff state.
// These pods cannot be restarted automatially due to known kubernetes issue https://github.com/kubernetes/kubernetes/issues/67250
func containsInvalidImage(cr argoproj.ArgoCD, r ReconcileArgoCD) (bool, error) {

	podList := &corev1.PodList{}
	applicationControllerListOption := client.MatchingLabels{common.ArgoCDKeyName: fmt.Sprintf("%s-%s", cr.Name, "application-controller")}

	if err := r.Client.List(context.TODO(), podList, applicationControllerListOption, client.InNamespace(cr.Namespace)); err != nil {
		log.Error(err, "Failed to list Pods")
		return false, err
	}

	if len(podList.Items) == 0 {
		// No pods, no work to do
		return false, nil
	}

	if len(podList.Items) != 1 {
		// There should only be 0 or 1. If this message is printed, it suggests a problem.
		log.Info("Unexpected number of pods in 'containsInvalidImage' pod list", "podListItems", fmt.Sprintf("%d", len(podList.Items)), "namespace", cr.Namespace)
		return false, nil
	}

	appControllerPod := podList.Items[0]

	if len(appControllerPod.Status.ContainerStatuses) == 0 {
		// No container statuses for application-controller, no work to do.
		return false, nil
	}

	brokenPod := false

	waitingState := appControllerPod.Status.ContainerStatuses[0].State.Waiting
	if waitingState != nil {

		waitingReason := waitingState.Reason
		if waitingReason == "ImagePullBackOff" || waitingReason == "ErrImagePull" {

			var containerImage string
			if len(appControllerPod.Spec.Containers) > 0 {
				containerImage = appControllerPod.Spec.Containers[0].Image
			}

			log.Info("A broken pod was detected", "waitingReason", waitingReason, "containerImage", containerImage)
			brokenPod = true
		}

	}

	return brokenPod, nil
}
