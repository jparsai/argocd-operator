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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"sigs.k8s.io/controller-runtime/pkg/builder"

	"gopkg.in/yaml.v2"

	"github.com/argoproj/argo-cd/v3/util/glob"
	"github.com/go-logr/logr"

	"github.com/argoproj-labs/argocd-operator/api/v1alpha1"
	argoproj "github.com/argoproj-labs/argocd-operator/api/v1beta1"
	"github.com/argoproj-labs/argocd-operator/common"
	"github.com/argoproj-labs/argocd-operator/controllers/argocdagent"
	"github.com/argoproj-labs/argocd-operator/controllers/argoutil"

	oappsv1 "github.com/openshift/api/apps/v1"
	configv1 "github.com/openshift/api/config/v1"
	routev1 "github.com/openshift/api/route/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/sethvargo/go-password/password"
	"golang.org/x/mod/semver"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	v1 "k8s.io/api/rbac/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	grafanaDeprecatedWarning     = "Warning: grafana field is deprecated from ArgoCD: field will be ignored."
	initialRepositoriesWarning   = "Warning: Argo CD InitialRepositories field is deprecated from ArgoCD, field will be ignored."
	repositoryCredentialsWarning = "Warning: Argo CD RepositoryCredentials field is deprecated from ArgoCD, field will be ignored."
)

var (
	versionAPIFound = false
)

// IsVersionAPIAvailable returns true if the version api is present
func IsVersionAPIAvailable() bool {
	return versionAPIFound
}

// verifyVersionAPI will verify that the template API is present.
func verifyVersionAPI() error {
	found, err := argoutil.VerifyAPI(configv1.GroupName, configv1.GroupVersion.Version)
	if err != nil {
		return err
	}
	versionAPIFound = found
	return nil
}

// generateArgoAdminPassword will generate and return the admin password for Argo CD.
func generateArgoAdminPassword() ([]byte, error) {
	pass, err := password.Generate(
		common.ArgoCDDefaultAdminPasswordLength,
		common.ArgoCDDefaultAdminPasswordNumDigits,
		common.ArgoCDDefaultAdminPasswordNumSymbols,
		false, false)

	return []byte(pass), err
}

// generateRedisAdminPassword will generate and return the admin password for Redis.
func generateRedisAdminPassword() ([]byte, error) {
	pass, err := password.Generate(
		common.RedisDefaultAdminPasswordLength,
		common.RedisDefaultAdminPasswordNumDigits,
		common.RedisDefaultAdminPasswordNumSymbols,
		false, false)

	return []byte(pass), err
}

// generateArgoServerKey will generate and return the server signature key for session validation.
func generateArgoServerSessionKey() ([]byte, error) {
	pass, err := password.Generate(
		common.ArgoCDDefaultServerSessionKeyLength,
		common.ArgoCDDefaultServerSessionKeyNumDigits,
		common.ArgoCDDefaultServerSessionKeyNumSymbols,
		false, false)

	return []byte(pass), err
}

// getArgoApplicationControllerResources will return the ResourceRequirements for the Argo CD application controller container.
func getArgoApplicationControllerResources(cr *argoproj.ArgoCD) corev1.ResourceRequirements {
	resources := corev1.ResourceRequirements{}

	// Allow override of resource requirements from CR
	if cr.Spec.Controller.Resources != nil {
		resources = *cr.Spec.Controller.Resources
	}

	return resources
}

// getArgoApplicationControllerCommand will return the command for the ArgoCD Application Controller component.
func getArgoApplicationControllerCommand(cr *argoproj.ArgoCD, useTLSForRedis bool) []string {
	cmd := []string{
		"argocd-application-controller",
		"--operation-processors", fmt.Sprint(getArgoServerOperationProcessors(cr)),
	}

	if cr.Spec.Redis.IsEnabled() {
		cmd = append(cmd, "--redis", getRedisServerAddress(cr))
	} else {
		log.Info("Redis is Disabled. Skipping adding Redis configuration to Application Controller.")
	}

	if useTLSForRedis {
		cmd = append(cmd, "--redis-use-tls")
		if isRedisTLSVerificationDisabled(cr) {
			cmd = append(cmd, "--redis-insecure-skip-tls-verify")
		} else {
			cmd = append(cmd, "--redis-ca-certificate", "/app/config/controller/tls/redis/tls.crt")
		}
	}

	if cr.Spec.Repo.IsEnabled() {
		cmd = append(cmd, "--repo-server", getRepoServerAddress(cr))
	} else {
		log.Info("Repo Server is disabled. This would affect the functioning of Application Controller.")
	}

	cmd = append(cmd, "--status-processors", fmt.Sprint(getArgoServerStatusProcessors(cr)))
	cmd = append(cmd, "--kubectl-parallelism-limit", fmt.Sprint(getArgoControllerParellismLimit(cr)))

	if len(cr.Spec.SourceNamespaces) > 0 {
		cmd = append(cmd, "--application-namespaces", fmt.Sprint(strings.Join(cr.Spec.SourceNamespaces, ",")))
	}

	cmd = append(cmd, "--loglevel")
	cmd = append(cmd, getLogLevel(cr.Spec.Controller.LogLevel))

	cmd = append(cmd, "--logformat")
	cmd = append(cmd, getLogFormat(cr.Spec.Controller.LogFormat))

	persistHealth := "true" // default
	if val, ok := cr.Spec.CmdParams["controller.resource.health.persist"]; ok {
		persistHealth = val
	}

	// set the command only if persistHealth is true
	if persistHealth == "true" {
		cmd = append(cmd, "--persist-resource-health")
	}

	// check if extra args are present
	extraArgs := cr.Spec.Controller.ExtraCommandArgs
	cmd = appendUniqueArgs(cmd, extraArgs)

	return cmd
}

// getArgoContainerImage will return the container image for ArgoCD.
func getArgoContainerImage(cr *argoproj.ArgoCD) string {
	defaultTag, defaultImg := false, false
	img := cr.Spec.Image
	if img == "" {
		img = common.ArgoCDDefaultArgoImage
		defaultImg = true
	}

	tag := cr.Spec.Version
	if tag == "" {
		tag = common.ArgoCDDefaultArgoVersion
		defaultTag = true
	}
	if e := os.Getenv(common.ArgoCDImageEnvName); e != "" && (defaultTag && defaultImg) {
		return e
	}

	return argoutil.CombineImageTag(img, tag)
}

// getRepoServerContainerImage will return the container image for the Repo server.
//
// There are four possible options for configuring the image, and this is the
// order of preference.
//
// 1. from the Spec, the spec.repo field has an image and version to use for
// generating an image reference.
// 2. from the Spec, the spec.version field has an image and version to use for
// generating an image reference
// 3. from the Environment, this looks for the `ARGOCD_IMAGE` field and uses
// that if the spec is not configured.
// 4. the default is configured in common.ArgoCDDefaultArgoVersion and
// common.ArgoCDDefaultArgoImage.
func getRepoServerContainerImage(cr *argoproj.ArgoCD) string {
	defaultImg, defaultTag := false, false
	img := cr.Spec.Repo.Image
	if img == "" {
		img = cr.Spec.Image
		if img == "" {
			img = common.ArgoCDDefaultArgoImage
			defaultImg = true
		}
	}

	tag := cr.Spec.Repo.Version
	if tag == "" {
		tag = cr.Spec.Version
		if tag == "" {
			tag = common.ArgoCDDefaultArgoVersion
			defaultTag = true
		}
	}
	if e := os.Getenv(common.ArgoCDImageEnvName); e != "" && (defaultTag && defaultImg) {
		return e
	}
	return argoutil.CombineImageTag(img, tag)
}

// getArgoRepoResources will return the ResourceRequirements for the Argo CD Repo server container.
func getArgoRepoResources(cr *argoproj.ArgoCD) corev1.ResourceRequirements {
	resources := corev1.ResourceRequirements{}

	// Allow override of resource requirements from CR
	if cr.Spec.Repo.Resources != nil {
		resources = *cr.Spec.Repo.Resources
	}

	return resources
}

// getArgoServerInsecure returns the insecure value for the ArgoCD Server component.
func getArgoServerInsecure(cr *argoproj.ArgoCD) bool {
	return cr.Spec.Server.Insecure
}

func isRepoServerTLSVerificationRequested(cr *argoproj.ArgoCD) bool {
	return cr.Spec.Repo.VerifyTLS
}

func isRedisTLSVerificationDisabled(cr *argoproj.ArgoCD) bool {
	return cr.Spec.Redis.DisableTLSVerification
}

// getArgoServerGRPCHost will return the GRPC host for the given ArgoCD.
func getArgoServerGRPCHost(cr *argoproj.ArgoCD) string {
	host := nameWithSuffix("grpc", cr)
	if len(cr.Spec.Server.GRPC.Host) > 0 {
		host = cr.Spec.Server.GRPC.Host
	}
	return host
}

// getArgoServerHost will return the host for the given ArgoCD.
func getArgoServerHost(cr *argoproj.ArgoCD) string {
	host := cr.Name
	if len(cr.Spec.Server.Host) > 0 {
		host = cr.Spec.Server.Host
	}
	return host
}

// getKeycloakIngressHost will return the host for the given ArgoCD.
func getKeycloakIngressHost(cr *argoproj.ArgoCDKeycloakSpec) string {
	if cr != nil && len(cr.Host) > 0 {
		return cr.Host
	}
	// If cr is nil or cr.Host is empty, return a default value or handle it accordingly.
	return keycloakIngressHost
}

// getKeycloakIngressHost will return the host for the given ArgoCD.
func getKeycloakOpenshiftHost(cr *argoproj.ArgoCDKeycloakSpec) string {
	if cr != nil && len(cr.Host) > 0 {
		return cr.Host
	}
	return ""
}

// getArgoServerResources will return the ResourceRequirements for the Argo CD server container.
func getArgoServerResources(cr *argoproj.ArgoCD) corev1.ResourceRequirements {
	resources := corev1.ResourceRequirements{}

	if cr.Spec.Server.Autoscale.Enabled {
		resources = corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(common.ArgoCDDefaultServerResourceLimitCPU),
				corev1.ResourceMemory: resource.MustParse(common.ArgoCDDefaultServerResourceLimitMemory),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(common.ArgoCDDefaultServerResourceRequestCPU),
				corev1.ResourceMemory: resource.MustParse(common.ArgoCDDefaultServerResourceRequestMemory),
			},
		}
	}

	// Allow override of resource requirements from CR
	if cr.Spec.Server.Resources != nil {
		resources = *cr.Spec.Server.Resources
	}

	return resources
}

// getArgoServerURI will return the URI for the ArgoCD server.
// The hostname for argocd-server is from the route, ingress, an external hostname or service name in that order.
func (r *ReconcileArgoCD) getArgoServerURI(cr *argoproj.ArgoCD) (string, error) {
	host := nameWithSuffix("server", cr) // Default to service name

	// Use the external hostname provided by the user
	if cr.Spec.Server.Host != "" {
		host = cr.Spec.Server.Host
	}

	// Use Ingress host if enabled
	if cr.Spec.Server.Ingress.Enabled {
		ing := newIngressWithSuffix("server", cr)
		ingressExists, err := argoutil.IsObjectFound(r.Client, cr.Namespace, ing.Name, ing)
		if err != nil {
			return "", err
		}
		if ingressExists {
			host = ing.Spec.Rules[0].Host
		}
	}

	if cr.Spec.Server.Route.Enabled && IsRouteAPIAvailable() {
		// Use Route host if available, override Ingress if both exist
		route := newRouteWithSuffix("server", cr)
		routeExists, err := argoutil.IsObjectFound(r.Client, cr.Namespace, route.Name, route)
		if err != nil {
			return "", err
		}
		if routeExists {
			host = route.Spec.Host
		}
	}

	return fmt.Sprintf("https://%s", host), nil // TODO: Safe to assume HTTPS here?
}

// getArgoServerOperationProcessors will return the numeric Operation Processors value for the ArgoCD Server.
func getArgoServerOperationProcessors(cr *argoproj.ArgoCD) int32 {
	op := common.ArgoCDDefaultServerOperationProcessors
	if cr.Spec.Controller.Processors.Operation > 0 {
		op = cr.Spec.Controller.Processors.Operation
	}
	return op
}

// getArgoServerStatusProcessors will return the numeric Status Processors value for the ArgoCD Server.
func getArgoServerStatusProcessors(cr *argoproj.ArgoCD) int32 {
	sp := common.ArgoCDDefaultServerStatusProcessors
	if cr.Spec.Controller.Processors.Status > 0 {
		sp = cr.Spec.Controller.Processors.Status
	}
	return sp
}

// getArgoControllerParellismLimit returns the parallelism limit for the application controller
func getArgoControllerParellismLimit(cr *argoproj.ArgoCD) int32 {
	pl := common.ArgoCDDefaultControllerParallelismLimit
	if cr.Spec.Controller.ParallelismLimit > 0 {
		pl = cr.Spec.Controller.ParallelismLimit
	}
	return pl
}

// getRedisConfigPath will return the path for the Redis configuration templates.
func getRedisConfigPath() string {
	path := os.Getenv("REDIS_CONFIG_PATH")
	if len(path) > 0 {
		return path
	}
	return common.ArgoCDDefaultRedisConfigPath
}

// getRedisInitScript will load the redis configuration from a template on disk for the given ArgoCD.
// If an error occurs, an empty string value will be returned.
func getRedisConf(useTLSForRedis bool) string {
	path := fmt.Sprintf("%s/redis.conf.tpl", getRedisConfigPath())
	params := map[string]string{
		"UseTLS": strconv.FormatBool(useTLSForRedis),
	}
	conf, err := loadTemplateFile(path, params)
	if err != nil {
		log.Error(err, "unable to load redis configuration")
		return ""
	}
	return conf
}

// getRedisContainerImage will return the container image for the Redis server.
func getRedisContainerImage(cr *argoproj.ArgoCD) string {
	defaultImg, defaultTag := false, false
	img := cr.Spec.Redis.Image
	if img == "" {
		img = common.ArgoCDDefaultRedisImage
		defaultImg = true
	}
	tag := cr.Spec.Redis.Version
	if tag == "" {
		tag = common.ArgoCDDefaultRedisVersion
		defaultTag = true
	}
	if e := os.Getenv(common.ArgoCDRedisImageEnvName); e != "" && (defaultTag && defaultImg) {
		return e
	}
	return argoutil.CombineImageTag(img, tag)
}

// getRedisHAContainerImage will return the container image for the Redis server in HA mode.
func getRedisHAContainerImage(cr *argoproj.ArgoCD) string {
	defaultImg, defaultTag := false, false
	img := cr.Spec.Redis.Image
	if img == "" {
		img = common.ArgoCDDefaultRedisImage
		defaultImg = true
	}
	tag := cr.Spec.Redis.Version
	if tag == "" {
		tag = common.ArgoCDDefaultRedisVersionHA
		defaultTag = true
	}
	if e := os.Getenv(common.ArgoCDRedisHAImageEnvName); e != "" && (defaultTag && defaultImg) {
		return e
	}
	return argoutil.CombineImageTag(img, tag)
}

// getRedisHAProxyAddress will return the Redis HA Proxy service address for the given ArgoCD.
func getRedisHAProxyAddress(cr *argoproj.ArgoCD) string {
	return fqdnServiceRef("redis-ha-haproxy", common.ArgoCDDefaultRedisPort, cr)
}

// getRedisHAProxyContainerImage will return the container image for the Redis HA Proxy.
func getRedisHAProxyContainerImage(cr *argoproj.ArgoCD) string {
	defaultImg, defaultTag := false, false
	img := cr.Spec.HA.RedisProxyImage
	if len(img) <= 0 {
		img = common.ArgoCDDefaultRedisHAProxyImage
		defaultImg = true
	}

	tag := cr.Spec.HA.RedisProxyVersion
	if len(tag) <= 0 {
		tag = common.ArgoCDDefaultRedisHAProxyVersion
		defaultTag = true
	}

	if e := os.Getenv(common.ArgoCDRedisHAProxyImageEnvName); e != "" && (defaultTag && defaultImg) {
		return e
	}

	return argoutil.CombineImageTag(img, tag)
}

// getRedisInitScript will load the redis init script from a template on disk for the given ArgoCD.
// If an error occurs, an empty string value will be returned.
func getRedisInitScript(cr *argoproj.ArgoCD, useTLSForRedis bool) string {
	path := fmt.Sprintf("%s/init.sh.tpl", getRedisConfigPath())
	vars := map[string]string{
		"ServiceName": nameWithSuffix("redis-ha", cr),
		"UseTLS":      strconv.FormatBool(useTLSForRedis),
	}

	script, err := loadTemplateFile(path, vars)
	if err != nil {
		log.Error(err, "unable to load redis init-script")
		return ""
	}
	return script
}

// getRedisHAProxySConfig will load the Redis HA Proxy configuration from a template on disk for the given ArgoCD.
// If an error occurs, an empty string value will be returned.
func getRedisHAProxyConfig(cr *argoproj.ArgoCD, useTLSForRedis bool) string {
	path := fmt.Sprintf("%s/haproxy.cfg.tpl", getRedisConfigPath())
	vars := map[string]string{
		"ServiceName": nameWithSuffix("redis-ha", cr),
		"UseTLS":      strconv.FormatBool(useTLSForRedis),
	}

	script, err := loadTemplateFile(path, vars)
	if err != nil {
		log.Error(err, "unable to load redis haproxy configuration")
		return ""
	}
	return script
}

// getRedisHAProxyScript will load the Redis HA Proxy init script from a template on disk for the given ArgoCD.
// If an error occurs, an empty string value will be returned.
func getRedisHAProxyScript(cr *argoproj.ArgoCD) string {
	path := fmt.Sprintf("%s/haproxy_init.sh.tpl", getRedisConfigPath())
	vars := map[string]string{
		"ServiceName": nameWithSuffix("redis-ha", cr),
	}

	script, err := loadTemplateFile(path, vars)
	if err != nil {
		log.Error(err, "unable to load redis haproxy init script")
		return ""
	}
	return script
}

// getRedisResources will return the ResourceRequirements for the Redis container.
func getRedisResources(cr *argoproj.ArgoCD) corev1.ResourceRequirements {
	resources := corev1.ResourceRequirements{}

	// Allow override of resource requirements from CR
	if cr.Spec.Redis.Resources != nil {
		resources = *cr.Spec.Redis.Resources
	}

	return resources
}

// getRedisHAResources will return the ResourceRequirements for the Redis HA.
func getRedisHAResources(cr *argoproj.ArgoCD) corev1.ResourceRequirements {
	resources := corev1.ResourceRequirements{}

	// Allow override of resource requirements from CR
	if cr.Spec.HA.Resources != nil {
		resources = *cr.Spec.HA.Resources
	}

	return resources
}

// getRedisSentinelConf will load the redis sentinel configuration from a template on disk for the given ArgoCD.
// If an error occurs, an empty string value will be returned.
func getRedisSentinelConf(useTLSForRedis bool) string {
	path := fmt.Sprintf("%s/sentinel.conf.tpl", getRedisConfigPath())
	params := map[string]string{
		"UseTLS": strconv.FormatBool(useTLSForRedis),
	}
	conf, err := loadTemplateFile(path, params)
	if err != nil {
		log.Error(err, "unable to load redis sentinel configuration")
		return ""
	}
	return conf
}

// getRedisLivenessScript will load the redis liveness script from a template on disk for the given ArgoCD.
// If an error occurs, an empty string value will be returned.
func getRedisLivenessScript(useTLSForRedis bool) string {
	path := fmt.Sprintf("%s/redis_liveness.sh.tpl", getRedisConfigPath())
	params := map[string]string{
		"UseTLS": strconv.FormatBool(useTLSForRedis),
	}
	conf, err := loadTemplateFile(path, params)
	if err != nil {
		log.Error(err, "unable to load redis liveness script")
		return ""
	}
	return conf
}

// getRedisReadinessScript will load the redis readiness script from a template on disk for the given ArgoCD.
// If an error occurs, an empty string value will be returned.
func getRedisReadinessScript(useTLSForRedis bool) string {
	path := fmt.Sprintf("%s/redis_readiness.sh.tpl", getRedisConfigPath())
	params := map[string]string{
		"UseTLS": strconv.FormatBool(useTLSForRedis),
	}
	conf, err := loadTemplateFile(path, params)
	if err != nil {
		log.Error(err, "unable to load redis readiness script")
		return ""
	}
	return conf
}

// getSentinelLivenessScript will load the redis liveness script from a template on disk for the given ArgoCD.
// If an error occurs, an empty string value will be returned.
func getSentinelLivenessScript(useTLSForRedis bool) string {
	path := fmt.Sprintf("%s/sentinel_liveness.sh.tpl", getRedisConfigPath())
	params := map[string]string{
		"UseTLS": strconv.FormatBool(useTLSForRedis),
	}
	conf, err := loadTemplateFile(path, params)
	if err != nil {
		log.Error(err, "unable to load sentinel liveness script")
		return ""
	}
	return conf
}

// getRedisServerAddress will return the Redis service address for the given ArgoCD.
func getRedisServerAddress(cr *argoproj.ArgoCD) string {
	if cr.Spec.Redis.Remote != nil && *cr.Spec.Redis.Remote != "" {
		return *cr.Spec.Redis.Remote
	}

	// If principal is enabled, then Argo CD server/repo server should be configured to use redis proxy from principal (argo cd agent)
	if cr.Spec.ArgoCDAgent != nil && cr.Spec.ArgoCDAgent.Principal != nil && cr.Spec.ArgoCDAgent.Principal.IsEnabled() {
		return argoutil.GenerateAgentPrincipalRedisProxyServiceName(cr.Name) + "." + cr.Namespace + ".svc.cluster.local:6379"
	}

	if cr.Spec.HA.Enabled {
		return getRedisHAProxyAddress(cr)
	}

	return fqdnServiceRef(common.ArgoCDDefaultRedisSuffix, common.ArgoCDDefaultRedisPort, cr)
}

// loadTemplateFile will parse a template with the given path and execute it with the given params.
func loadTemplateFile(path string, params map[string]string) (string, error) {
	tmpl, err := template.ParseFiles(path)
	if err != nil {
		log.Error(err, "unable to parse template")
		return "", fmt.Errorf("unable to parse template. error: %w", err)
	}

	buf := new(bytes.Buffer)
	err = tmpl.Execute(buf, params)
	if err != nil {
		log.Error(err, "unable to execute template")
		return "", fmt.Errorf("unable to execute template. error: %w", err)
	}
	return buf.String(), nil
}

// nameWithSuffix will return a name based on the given ArgoCD. The given suffix is appended to the generated name.
// Example: Given an ArgoCD with the name "example-argocd", providing the suffix "foo" would result in the value of
// "example-argocd-foo" being returned.
func nameWithSuffix(suffix string, cr *argoproj.ArgoCD) string {
	return fmt.Sprintf("%s-%s", cr.Name, suffix)
}

// fqdnServiceRef will return the FQDN referencing a specific service name, as set up by the operator, with the
// given port.
func fqdnServiceRef(service string, port int, cr *argoproj.ArgoCD) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", nameWithSuffix(service, cr), cr.Namespace, port)
}

// InspectCluster will verify the availability of extra features available to the cluster, such as Prometheus and
// OpenShift Routes.
func InspectCluster() error {
	if err := verifyPrometheusAPI(); err != nil {
		return err
	}

	if err := verifyRouteAPI(); err != nil {
		return err
	}

	if err := verifyKeycloakTemplateAPIs(); err != nil {
		return err
	}

	if err := verifyVersionAPI(); err != nil {
		return err
	}
	return nil
}

// reconcileCertificateAuthority will reconcile all Certificate Authority resources.
func (r *ReconcileArgoCD) reconcileCertificateAuthority(cr *argoproj.ArgoCD) error {
	log.Info("reconciling CA secret")
	if err := r.reconcileClusterCASecret(cr); err != nil {
		return err
	}

	log.Info("reconciling CA config map")
	if err := r.reconcileCAConfigMap(cr); err != nil {
		return err
	}
	return nil
}

func (r *ReconcileArgoCD) redisShouldUseTLS(cr *argoproj.ArgoCD) bool {
	var tlsSecretObj corev1.Secret
	tlsSecretName := types.NamespacedName{Namespace: cr.Namespace, Name: common.ArgoCDRedisServerTLSSecretName}
	err := r.Client.Get(context.TODO(), tlsSecretName, &tlsSecretObj)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "error looking up redis tls secret")
		}
		return false
	}

	secretOwnerRefs := tlsSecretObj.GetOwnerReferences()
	if len(secretOwnerRefs) > 0 {
		// OpenShift service CA makes the owner reference for the TLS secret to the
		// service, which in turn is owned by the controller. This method performs
		// a lookup of the controller through the intermediate owning service.
		for _, secretOwner := range secretOwnerRefs {
			if isOwnerOfInterest(secretOwner) {
				key := client.ObjectKey{Name: secretOwner.Name, Namespace: tlsSecretObj.GetNamespace()}
				svc := &corev1.Service{}

				// Get the owning object of the secret
				err := r.Client.Get(context.TODO(), key, svc)
				if err != nil {
					log.Error(err, fmt.Sprintf("could not get owner of secret %s", tlsSecretObj.GetName()))
					return false
				}

				// If there's an object of kind ArgoCD in the owner's list,
				// this will be our reconciled object.
				serviceOwnerRefs := svc.GetOwnerReferences()
				for _, serviceOwner := range serviceOwnerRefs {
					if serviceOwner.Kind == "ArgoCD" {
						return true
					}
				}
			}
		}
	} else {
		// For secrets without owner (i.e. manually created), we apply some
		// heuristics. This may not be as accurate (e.g. if the user made a
		// typo in the resource's name), but should be good enough for now.
		if _, ok := tlsSecretObj.Annotations[common.AnnotationName]; ok {
			return true
		}
	}
	return false
}

// reconcileResources will reconcile common ArgoCD resources.
func (r *ReconcileArgoCD) reconcileResources(cr *argoproj.ArgoCD) error {

	log.Info("reconciling status")
	if err := r.reconcileStatus(cr); err != nil {
		log.Info(err.Error())
	}

	log.Info("reconciling SSO")
	if err := r.reconcileSSO(cr); err != nil {
		log.Info(err.Error())
		return err
	}

	log.Info("reconciling roles")
	if err := r.reconcileRoles(cr); err != nil {
		log.Info(err.Error())
		return err
	}

	log.Info("reconciling rolebindings")
	if err := r.reconcileRoleBindings(cr); err != nil {
		log.Info(err.Error())
		return err
	}

	log.Info("reconciling service accounts")
	if err := r.reconcileServiceAccounts(cr); err != nil {
		log.Info(err.Error())
		return err
	}

	log.Info("reconciling certificate authority")
	if err := r.reconcileCertificateAuthority(cr); err != nil {
		return err
	}

	log.Info("reconciling secrets")
	if err := r.reconcileSecrets(cr); err != nil {
		return err
	}

	useTLSForRedis := r.redisShouldUseTLS(cr)

	log.Info("reconciling config maps")
	if err := r.reconcileConfigMaps(cr, useTLSForRedis); err != nil {
		return err
	}

	log.Info("reconciling services")
	if err := r.reconcileServices(cr); err != nil {
		return err
	}

	log.Info("reconciling deployments")
	if err := r.reconcileDeployments(cr, useTLSForRedis); err != nil {
		return err
	}

	log.Info("reconciling statefulsets")
	if err := r.reconcileStatefulSets(cr, useTLSForRedis); err != nil {
		return err
	}

	log.Info("reconciling autoscalers")
	if err := r.reconcileAutoscalers(cr); err != nil {
		return err
	}

	log.Info("reconciling ingresses")
	if err := r.reconcileIngresses(cr); err != nil {
		return err
	}

	if IsRouteAPIAvailable() {
		log.Info("reconciling routes")
		if err := r.reconcileRoutes(cr); err != nil {
			return err
		}
	}

	if IsPrometheusAPIAvailable() {
		log.Info("reconciling prometheus")
		if err := r.reconcilePrometheus(cr); err != nil {
			return err
		}

		// Reconciles prometheusRule created to alert based on argo-cd workload status
		if err := r.reconcilePrometheusRule(cr); err != nil {
			return err
		}

		if err := r.reconcileMetricsServiceMonitor(cr); err != nil {
			return err
		}

		if err := r.reconcileRepoServerServiceMonitor(cr); err != nil {
			return err
		}

		if err := r.reconcileServerMetricsServiceMonitor(cr); err != nil {
			return err
		}
	}

	// check ManagedApplicationSetSourceNamespaces for proper cleanup
	if cr.Spec.ApplicationSet != nil || len(r.ManagedApplicationSetSourceNamespaces) > 0 {
		log.Info("reconciling ApplicationSet controller")
		if err := r.reconcileApplicationSetController(cr); err != nil {
			return err
		}
	}

	if cr.Spec.Notifications.Enabled {
		log.Info("reconciling Notifications controller")
		if err := r.reconcileNotificationsController(cr); err != nil {
			return err
		}
	}

	if err := r.reconcileRepoServerTLSSecret(cr); err != nil {
		return err
	}

	if err := r.reconcileRedisTLSSecret(cr, useTLSForRedis); err != nil {
		return err
	}

	if err := r.ReconcileNetworkPolicies(cr); err != nil {
		return err
	}

	if err := r.reconcileArgoCDAgent(cr); err != nil {
		return err
	}

	return nil
}

func (r *ReconcileArgoCD) deleteClusterResources(cr *argoproj.ArgoCD) error {
	selector, err := argocdInstanceSelector(cr.Name)
	if err != nil {
		return err
	}

	clusterRoleList := &v1.ClusterRoleList{}
	if err := filterObjectsBySelector(r.Client, clusterRoleList, selector); err != nil {
		return fmt.Errorf("failed to filter ClusterRoles for %s: %w", cr.Name, err)
	}

	if err := deleteClusterRoles(r.Client, clusterRoleList); err != nil {
		return err
	}

	clusterBindingsList := &v1.ClusterRoleBindingList{}
	if err := filterObjectsBySelector(r.Client, clusterBindingsList, selector); err != nil {
		return fmt.Errorf("failed to filter ClusterRoleBindings for %s: %w", cr.Name, err)
	}

	if err := deleteClusterRoleBindings(r.Client, clusterBindingsList); err != nil {
		return err
	}

	return nil
}

func (r *ReconcileArgoCD) removeManagedByLabelFromNamespaces(namespace string) error {
	nsList := &corev1.NamespaceList{}
	listOption := client.MatchingLabels{
		common.ArgoCDManagedByLabel: namespace,
	}
	if err := r.Client.List(context.TODO(), nsList, listOption); err != nil {
		return err
	}

	nsList.Items = append(nsList.Items, corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
	for _, n := range nsList.Items {
		ns := &corev1.Namespace{}
		if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: n.Name}, ns); err != nil {
			return err
		}

		if ns.Labels == nil {
			continue
		}

		if n, ok := ns.Labels[common.ArgoCDManagedByLabel]; !ok || n != namespace {
			continue
		}
		delete(ns.Labels, common.ArgoCDManagedByLabel)
		argoutil.LogResourceUpdate(log, ns, "removing 'managed-by' label")
		if err := r.Client.Update(context.TODO(), ns); err != nil {
			log.Error(err, fmt.Sprintf("failed to remove label from namespace [%s]", ns.Name))
		}
	}
	return nil
}

func filterObjectsBySelector(c client.Client, objectList client.ObjectList, selector labels.Selector) error {
	return c.List(context.TODO(), objectList, client.MatchingLabelsSelector{Selector: selector})
}

func argocdInstanceSelector(name string) (labels.Selector, error) {
	selector := labels.NewSelector()
	requirement, err := labels.NewRequirement(common.ArgoCDKeyManagedBy, selection.Equals, []string{name})
	if err != nil {
		return nil, fmt.Errorf("failed to create a requirement for %w", err)
	}
	return selector.Add(*requirement), nil
}

func (r *ReconcileArgoCD) removeDeletionFinalizer(argocd *argoproj.ArgoCD) error {
	argocd.Finalizers = removeString(argocd.GetFinalizers(), common.ArgoCDDeletionFinalizer)
	argoutil.LogResourceUpdate(log, argocd, "removing deletion finalizer")
	if err := r.Client.Update(context.TODO(), argocd); err != nil {
		return fmt.Errorf("failed to remove deletion finalizer from %s: %w", argocd.Name, err)
	}
	return nil
}

func (r *ReconcileArgoCD) addDeletionFinalizer(argocd *argoproj.ArgoCD) error {
	argocd.Finalizers = append(argocd.Finalizers, common.ArgoCDDeletionFinalizer)
	argoutil.LogResourceUpdate(log, argocd, "adding deletion finalizer")
	if err := r.Client.Update(context.TODO(), argocd); err != nil {
		return fmt.Errorf("failed to add deletion finalizer for %s: %w", argocd.Name, err)
	}
	return nil
}

func removeString(slice []string, s string) []string {
	var result []string
	for _, item := range slice {
		if item == s {
			continue
		}
		result = append(result, item)
	}
	return result
}

// setResourceWatches will register Watches for each of the supported Resources.
func (r *ReconcileArgoCD) setResourceWatches(bldr *builder.Builder, clusterResourceMapper, tlsSecretMapper, namespaceResourceMapper, clusterSecretResourceMapper, applicationSetGitlabSCMTLSConfigMapMapper, nmMapper handler.MapFunc) *builder.Builder {

	deploymentConfigPred := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			// Ignore updates to CR status in which case metadata.Generation does not change
			var count int32 = 1
			newDC, ok := e.ObjectNew.(*oappsv1.DeploymentConfig)
			if !ok {
				return false
			}
			oldDC, ok := e.ObjectOld.(*oappsv1.DeploymentConfig)
			if !ok {
				return false
			}
			if newDC.Name == defaultKeycloakIdentifier {
				if newDC.Status.AvailableReplicas == count {
					return true
				}
				if newDC.Status.AvailableReplicas == int32(0) &&
					!reflect.DeepEqual(oldDC.Status.AvailableReplicas, newDC.Status.AvailableReplicas) {
					// Handle the deletion of keycloak pod.
					log.Info(fmt.Sprintf("Handle the pod deletion event for keycloak deployment config %s in namespace %s",
						newDC.Name, newDC.Namespace))
					err := handleKeycloakPodDeletion(newDC)
					if err != nil {
						log.Error(err, fmt.Sprintf("Failed to update Deployment Config %s for keycloak pod deletion in namespace %s",
							newDC.Name, newDC.Namespace))
					}
				}
			}
			return false
		},
	}

	deleteSSOPred := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			newCR, ok := e.ObjectNew.(*argoproj.ArgoCD)
			if !ok {
				return false
			}
			oldCR, ok := e.ObjectOld.(*argoproj.ArgoCD)
			if !ok {
				return false
			}

			// Handle deletion of SSO from Argo CD custom resource
			if !reflect.DeepEqual(oldCR.Spec.SSO, newCR.Spec.SSO) && newCR.Spec.SSO == nil {
				err := r.deleteSSOConfiguration(newCR, oldCR)
				if err != nil {
					log.Error(err, fmt.Sprintf("Failed to delete SSO Configuration for ArgoCD %s in namespace %s",
						newCR.Name, newCR.Namespace))
				}
			}

			// Trigger reconciliation of SSO on update event
			if !reflect.DeepEqual(oldCR.Spec.SSO, newCR.Spec.SSO) && newCR.Spec.SSO != nil && oldCR.Spec.SSO != nil {
				err := r.reconcileSSO(newCR)
				if err != nil {
					log.Error(err, fmt.Sprintf("Failed to update existing SSO Configuration for ArgoCD %s in namespace %s",
						newCR.Name, newCR.Namespace))
				}
			}
			return true
		},
	}

	// Add new predicate to delete Notifications Resources. The predicate watches the Argo CD CR for changes to the `.spec.Notifications.Enabled`
	// field. When a change is detected that results in notifications being disabled, we trigger deletion of notifications resources
	deleteNotificationsPred := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			newCR, ok := e.ObjectNew.(*argoproj.ArgoCD)
			if !ok {
				return false
			}
			oldCR, ok := e.ObjectOld.(*argoproj.ArgoCD)
			if !ok {
				return false
			}
			if oldCR.Spec.Notifications.Enabled && !newCR.Spec.Notifications.Enabled {
				err := r.deleteNotificationsResources(newCR)
				if err != nil {
					log.Error(err, fmt.Sprintf("Failed to delete notifications controller resources for ArgoCD %s in namespace %s",
						newCR.Name, newCR.Namespace))
				}
			}
			return true
		},
	}

	// Watch for changes to primary resource ArgoCD
	bldr.For(&argoproj.ArgoCD{}, builder.WithPredicates(deleteSSOPred, deleteNotificationsPred, r.argoCDNamespaceManagementFilterPredicate()))

	// Watch for changes to ConfigMap sub-resources owned by ArgoCD instances.
	bldr.Owns(&corev1.ConfigMap{})

	// Watch for changes to Secret sub-resources owned by ArgoCD instances.
	bldr.Owns(&corev1.Secret{})

	// Watch for changes to Service sub-resources owned by ArgoCD instances.
	bldr.Owns(&corev1.Service{})

	// Watch for changes to Deployment sub-resources owned by ArgoCD instances.
	bldr.Owns(&appsv1.Deployment{})

	// Watch for changes to Ingress sub-resources owned by ArgoCD instances.
	bldr.Owns(&networkingv1.Ingress{})

	bldr.Owns(&v1.Role{})

	bldr.Owns(&v1.RoleBinding{})

	nmMapperResourceHandler := handler.EnqueueRequestsFromMapFunc(nmMapper)

	bldr.Watches(&argoproj.NamespaceManagement{}, nmMapperResourceHandler, builder.WithPredicates(r.namespaceManagementFilterPredicate()))

	clusterResourceHandler := handler.EnqueueRequestsFromMapFunc(clusterResourceMapper)

	clusterSecretResourceHandler := handler.EnqueueRequestsFromMapFunc(clusterSecretResourceMapper)

	appSetGitlabSCMTLSConfigMapHandler := handler.EnqueueRequestsFromMapFunc(applicationSetGitlabSCMTLSConfigMapMapper)

	tlsSecretHandler := handler.EnqueueRequestsFromMapFunc(tlsSecretMapper)

	bldr.Watches(&v1.ClusterRoleBinding{}, clusterResourceHandler)

	bldr.Watches(&v1.ClusterRole{}, clusterResourceHandler)

	bldr.Watches(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: common.ArgoCDAppSetGitlabSCMTLSCertsConfigMapName,
	}}, appSetGitlabSCMTLSConfigMapHandler)

	// Watch for secrets of type TLS that might be created by external processes
	bldr.Watches(&corev1.Secret{Type: corev1.SecretTypeTLS}, tlsSecretHandler)

	// Watch for cluster secrets added to the argocd instance
	bldr.Watches(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{
			common.ArgoCDManagedByClusterArgoCDLabel: "cluster",
		}}}, clusterSecretResourceHandler)

	// Watch for changes to Secret sub-resources owned by ArgoCD instances.
	bldr.Owns(&appsv1.StatefulSet{})

	// Inspect cluster to verify availability of extra features
	// This sets the flags that are used in subsequent checks
	if err := InspectCluster(); err != nil {
		log.Info("unable to inspect cluster")
	}

	if IsRouteAPIAvailable() {
		// Watch OpenShift Route sub-resources owned by ArgoCD instances.
		bldr.Owns(&routev1.Route{})
	}

	if IsPrometheusAPIAvailable() {
		// Watch Prometheus sub-resources owned by ArgoCD instances.
		bldr.Owns(&monitoringv1.Prometheus{})

		// Watch Prometheus ServiceMonitor sub-resources owned by ArgoCD instances.
		bldr.Owns(&monitoringv1.ServiceMonitor{})
	}

	if CanUseKeycloakWithTemplate() {
		// Watch for the changes to Deployment Config
		bldr.Owns(&oappsv1.DeploymentConfig{}, builder.WithPredicates(deploymentConfigPred))

	}

	// Watch for changes to NotificationsConfiguration CR
	bldr.Owns(&v1alpha1.NotificationsConfiguration{})

	namespaceHandler := handler.EnqueueRequestsFromMapFunc(namespaceResourceMapper)

	bldr.Watches(&corev1.Namespace{}, namespaceHandler, builder.WithPredicates(r.namespaceFilterPredicate()))

	bldrHook := newBuilderHook(r.Client, bldr)
	err := applyReconcilerHook(&argoproj.ArgoCD{}, bldrHook, "")
	if err != nil {
		log.Error(err, "failed to apply builder hook")
	}

	return bldr
}

// boolPtr returns a pointer to val
func boolPtr(val bool) *bool {
	return &val
}

func int64Ptr(val int64) *int64 {
	return &val
}

// triggerRollout will trigger a rollout of a Kubernetes resource specified as
// obj. It currently supports Deployment and StatefulSet resources.
func (r *ReconcileArgoCD) triggerRollout(obj interface{}, key string) error {
	switch res := obj.(type) {
	case *appsv1.Deployment:
		return r.triggerDeploymentRollout(res, key)
	case *appsv1.StatefulSet:
		return r.triggerStatefulSetRollout(res, key)
	default:
		return fmt.Errorf("resource of unknown type %T, cannot trigger rollout", res)
	}
}

func allowedNamespace(current string, namespaces string) bool {

	clusterConfigNamespaces := splitList(namespaces)
	if len(clusterConfigNamespaces) > 0 {
		if clusterConfigNamespaces[0] == "*" {
			return true
		}

		for _, n := range clusterConfigNamespaces {
			if n == current {
				return true
			}
		}
	}
	return false
}

func splitList(s string) []string {
	elems := strings.Split(s, ",")
	for i := range elems {
		elems[i] = strings.TrimSpace(elems[i])
	}
	return elems
}

// DeprecationEventEmissionStatus is meant to track which deprecation events have been emitted already. This is temporary and can be removed in v0.0.6 once we have provided enough
// deprecation notice
type DeprecationEventEmissionStatus struct {
	SSOSpecDeprecationWarningEmitted    bool
	DexSpecDeprecationWarningEmitted    bool
	DisableDexDeprecationWarningEmitted bool
	TLSInsecureWarningEmitted           bool
}

// DeprecationEventEmissionTracker map stores the namespace containing ArgoCD instance as key and DeprecationEventEmissionStatus as value,
// where DeprecationEventEmissionStatus tracks the events that have been emitted for the instance in the particular namespace.
// This is temporary and can be removed in v0.0.6 when we remove the deprecated fields.
var DeprecationEventEmissionTracker = make(map[string]DeprecationEventEmissionStatus)

func (r *ReconcileArgoCD) namespaceFilterPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			// This checks if ArgoCDManagedByLabel exists in newMeta, if exists then -
			// 1. Check if oldMeta had the label or not? if no, return true
			// 2. if yes, check if the old and new values are different, if yes,
			// first deleteRBACs for the old value & return true.
			// Event is then handled by the reconciler, which would create appropriate RBACs.
			if valNew, ok := e.ObjectNew.GetLabels()[common.ArgoCDManagedByLabel]; ok {
				if valOld, ok := e.ObjectOld.GetLabels()[common.ArgoCDManagedByLabel]; ok && valOld != valNew {
					k8sClient := r.K8sClient
					if err := deleteRBACsForNamespace(e.ObjectOld.GetName(), k8sClient); err != nil {
						log.Error(err, fmt.Sprintf("failed to delete RBACs for namespace: %s", e.ObjectOld.GetName()))
					} else {
						log.Info(fmt.Sprintf("Successfully removed the RBACs for namespace: %s", e.ObjectOld.GetName()))
					}

					// Delete namespace from cluster secret of previously managing argocd instance
					if err := deleteManagedNamespaceFromClusterSecret(valOld, e.ObjectOld.GetName(), k8sClient); err != nil {
						log.Error(err, fmt.Sprintf("unable to delete namespace %s from cluster secret", e.ObjectOld.GetName()))
					} else {
						log.Info(fmt.Sprintf("Successfully deleted namespace %s from cluster secret", e.ObjectOld.GetName()))
					}
				}
				return true
			}
			// This checks if the old meta had the label, if it did, delete the RBACs for the namespace
			// which were created when the label was added to the namespace.
			if ns, ok := e.ObjectOld.GetLabels()[common.ArgoCDManagedByLabel]; ok && ns != "" {
				k8sClient := r.K8sClient
				if err := deleteRBACsForNamespace(e.ObjectOld.GetName(), k8sClient); err != nil {
					log.Error(err, fmt.Sprintf("failed to delete RBACs for namespace: %s", e.ObjectOld.GetName()))
				} else {
					log.Info(fmt.Sprintf("Successfully removed the RBACs for namespace: %s", e.ObjectOld.GetName()))
				}

				// Delete managed namespace from cluster secret
				if err := deleteManagedNamespaceFromClusterSecret(ns, e.ObjectOld.GetName(), k8sClient); err != nil {
					log.Error(err, fmt.Sprintf("unable to delete namespace %s from cluster secret", e.ObjectOld.GetName()))
				} else {
					log.Info(fmt.Sprintf("Successfully deleted namespace %s from cluster secret", e.ObjectOld.GetName()))
				}

			}
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			if ns, ok := e.Object.GetLabels()[common.ArgoCDManagedByLabel]; ok && ns != "" {
				k8sClient := r.K8sClient
				// Delete managed namespace from cluster secret
				err := deleteManagedNamespaceFromClusterSecret(ns, e.Object.GetName(), k8sClient)
				if err != nil {
					log.Error(err, fmt.Sprintf("unable to delete namespace %s from cluster secret", e.Object.GetName()))
				} else {
					log.Info(fmt.Sprintf("Successfully deleted namespace %s from cluster secret", e.Object.GetName()))
				}
			}

			// if a namespace is deleted, remove it from deprecationEventEmissionTracker (if exists) so that if a namespace with the same name
			// is created in the future and contains an Argo CD instance, it will be tracked appropriately
			delete(DeprecationEventEmissionTracker, e.Object.GetName())
			return false
		},
	}
}

// deleteRBACsForNamespace deletes the RBACs when the label from the namespace is removed.
func deleteRBACsForNamespace(sourceNS string, k8sClient kubernetes.Interface) error {
	log.Info(fmt.Sprintf("Removing the RBACs created for the namespace: %s", sourceNS))

	// List all the roles created for ArgoCD using the label selector
	labelSelector := metav1.LabelSelector{MatchLabels: map[string]string{common.ArgoCDKeyPartOf: common.ArgoCDAppName}}
	roles, err := k8sClient.RbacV1().Roles(sourceNS).List(context.TODO(), metav1.ListOptions{LabelSelector: labels.Set(labelSelector.MatchLabels).String()})
	if err != nil {
		message := fmt.Sprintf("failed to list roles for namespace: %s", sourceNS)
		log.Error(err, message)
		return fmt.Errorf("%s error: %w", message, err)
	}

	// Delete all the retrieved roles
	for _, role := range roles.Items {
		argoutil.LogResourceDeletion(log, &role)
		err = k8sClient.RbacV1().Roles(sourceNS).Delete(context.TODO(), role.Name, metav1.DeleteOptions{})
		if err != nil {
			log.Error(err, fmt.Sprintf("failed to delete roles for namespace: %s", sourceNS))
		}
	}

	// List all the roles bindings created for ArgoCD using the label selector
	roleBindings, err := k8sClient.RbacV1().RoleBindings(sourceNS).List(context.TODO(), metav1.ListOptions{LabelSelector: labels.Set(labelSelector.MatchLabels).String()})
	if err != nil {
		message := fmt.Sprintf("failed to list role bindings for namespace: %s", sourceNS)
		log.Error(err, message)
		return fmt.Errorf("%s error: %w", message, err)
	}

	// Delete all the retrieved role bindings
	for _, roleBinding := range roleBindings.Items {
		argoutil.LogResourceDeletion(log, &roleBinding)
		err = k8sClient.RbacV1().RoleBindings(sourceNS).Delete(context.TODO(), roleBinding.Name, metav1.DeleteOptions{})
		if err != nil {
			log.Error(err, fmt.Sprintf("failed to delete role binding for namespace: %s", sourceNS))
		}
	}

	return nil
}

func deleteManagedNamespaceFromClusterSecret(ownerNS, sourceNS string, k8sClient kubernetes.Interface) error {

	// Get the cluster secret used for configuring ArgoCD
	labelSelector := metav1.LabelSelector{MatchLabels: map[string]string{common.ArgoCDSecretTypeLabel: "cluster"}}
	secrets, err := k8sClient.CoreV1().Secrets(ownerNS).List(context.TODO(), metav1.ListOptions{LabelSelector: labels.Set(labelSelector.MatchLabels).String()})
	if err != nil {
		message := fmt.Sprintf("failed to retrieve secrets for namespace: %s", ownerNS)
		log.Error(err, message)
		return fmt.Errorf("%s error: %w", message, err)
	}

	// Find the cluster secret in the list that points to  common.ArgoCDDefaultServer (default server address)
	var localClusterSecret *corev1.Secret
	for x, clusterSecret := range secrets.Items {

		// check if cluster secret with default server address exists
		if string(clusterSecret.Data["server"]) == common.ArgoCDDefaultServer {
			localClusterSecret = &secrets.Items[x]
		}
	}

	if localClusterSecret == nil {
		// The Secret doesn't exist: no more work to do.
		return nil
	}

	// If the Secret doesn't even have a 'namespaces' field, we are done.
	oldNamespacesFromClusterSecret, ok := localClusterSecret.Data["namespaces"]
	if !ok {
		return nil
	}

	oldNamespaceListFromClusterSecret := strings.Split(string(oldNamespacesFromClusterSecret), ",")
	var newNamespaceList []string

	for _, n := range oldNamespaceListFromClusterSecret {
		// remove the namespace from the list of namespaces
		if strings.TrimSpace(n) == sourceNS {
			continue
		}
		newNamespaceList = append(newNamespaceList, strings.TrimSpace(n))
	}
	sort.Strings(newNamespaceList)
	newNamespaceListString := strings.Join(newNamespaceList, ",")

	// If the namespace list has changed, update the cluster secret
	if string(oldNamespacesFromClusterSecret) != newNamespaceListString {

		localClusterSecret.Data["namespaces"] = []byte(newNamespaceListString)

		// Update the secret with the updated list of namespaces
		argoutil.LogResourceUpdate(log, localClusterSecret, "removing managed namespace", sourceNS)
		if _, err = k8sClient.CoreV1().Secrets(ownerNS).Update(context.TODO(), localClusterSecret, metav1.UpdateOptions{}); err != nil {
			message := fmt.Sprintf("failed to update cluster permission secret for namespace: %s", ownerNS)
			log.Error(err, message)
			return fmt.Errorf("%s error: %w", message, err)
		}
	}

	return nil
}

// getLogLevel returns the log level for a specified component if it is set or returns the default log level if it is not set
func getLogLevel(logField string) string {

	switch strings.ToLower(logField) {
	case "debug",
		"info",
		"warn",
		"error":
		return logField
	}
	return common.ArgoCDDefaultLogLevel
}

// getLogFormat returns the log format for a specified component if it is set or returns the default log format if it is not set
func getLogFormat(logField string) string {
	switch strings.ToLower(logField) {
	case "text",
		"json":
		return logField
	}
	return common.ArgoCDDefaultLogFormat
}

func (r *ReconcileArgoCD) setManagedNamespaces(cr *argoproj.ArgoCD) error {
	namespaces := &corev1.NamespaceList{}
	listOption := client.MatchingLabels{
		common.ArgoCDManagedByLabel: cr.Namespace,
	}

	// get the list of namespaces managed by the Argo CD instance
	if err := r.Client.List(context.TODO(), namespaces, listOption); err != nil {
		return err
	}

	namespaces.Items = append(namespaces.Items, corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cr.Namespace}})
	r.ManagedNamespaces = namespaces
	return nil
}

// getSourceNamespaces retrieves a list of namespaces that match the sourceNamespaces
// pattern specified in the given ArgoCD
func (r *ReconcileArgoCD) getSourceNamespaces(cr *argoproj.ArgoCD) ([]string, error) {
	sourceNamespaces := []string{}
	namespaces := &corev1.NamespaceList{}

	if err := r.Client.List(context.TODO(), namespaces, &client.ListOptions{}); err != nil {
		return nil, err
	}

	for _, namespace := range namespaces.Items {
		if glob.MatchStringInList(cr.Spec.SourceNamespaces, namespace.Name, glob.REGEXP) {
			sourceNamespaces = append(sourceNamespaces, namespace.Name)
		}
	}

	return sourceNamespaces, nil
}

func (r *ReconcileArgoCD) setManagedSourceNamespaces(cr *argoproj.ArgoCD) error {
	r.ManagedSourceNamespaces = make(map[string]string)
	namespaces := &corev1.NamespaceList{}
	listOption := client.MatchingLabels{
		common.ArgoCDManagedByClusterArgoCDLabel: cr.Namespace,
	}

	// get the list of namespaces managed by the Argo CD instance
	if err := r.Client.List(context.TODO(), namespaces, listOption); err != nil {
		return err
	}

	for _, namespace := range namespaces.Items {
		r.ManagedSourceNamespaces[namespace.Name] = ""
	}

	return nil
}

// removeUnmanagedSourceNamespaceResources cleansup resources from SourceNamespaces if namespace is not managed by argocd instance.
// It also removes the managed-by-cluster-argocd label from the namespace
func (r *ReconcileArgoCD) removeUnmanagedSourceNamespaceResources(cr *argoproj.ArgoCD) error {

	for ns := range r.ManagedSourceNamespaces {
		managedNamespace := false
		if cr.GetDeletionTimestamp() == nil {
			sourceNamespaces, err := r.getSourceNamespaces(cr)
			if err != nil {
				return err
			}
			for _, namespace := range sourceNamespaces {
				if namespace == ns {
					managedNamespace = true
					break
				}
			}
		}

		if !managedNamespace {
			if err := r.cleanupUnmanagedSourceNamespaceResources(cr, ns); err != nil {
				log.Error(err, fmt.Sprintf("error cleaning up resources for namespace %s", ns))
				continue
			}
			delete(r.ManagedSourceNamespaces, ns)
		}
	}
	return nil
}

func (r *ReconcileArgoCD) cleanupUnmanagedSourceNamespaceResources(cr *argoproj.ArgoCD, ns string) error {
	namespace := corev1.Namespace{}
	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: ns}, &namespace); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	// Remove managed-by-cluster-argocd from the namespace
	delete(namespace.Labels, common.ArgoCDManagedByClusterArgoCDLabel)
	argoutil.LogResourceUpdate(log, &namespace, "removing 'managed-by-cluster-argocd' label from umanaged source namespace")
	if err := r.Client.Update(context.TODO(), &namespace); err != nil {
		log.Error(err, fmt.Sprintf("failed to remove label from namespace [%s]", namespace.Name))
	}

	// Delete Roles for SourceNamespaces
	existingRole := v1.Role{}
	roleName := getRoleNameForApplicationSourceNamespaces(namespace.Name, cr)
	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: roleName, Namespace: namespace.Name}, &existingRole); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to fetch the role for the service account associated with %s : %s", common.ArgoCDServerComponent, err)
		}
	}
	if existingRole.Name != "" {
		argoutil.LogResourceDeletion(log, &existingRole, "cleaning up unmanaged source namespace")
		if err := r.Client.Delete(context.TODO(), &existingRole); err != nil {
			return err
		}
	}
	// Delete RoleBindings for SourceNamespaces
	existingRoleBinding := &v1.RoleBinding{}
	roleBindingName := getRoleBindingNameForSourceNamespaces(cr.Name, namespace.Name)
	if err := r.Client.Get(context.TODO(), types.NamespacedName{Name: roleBindingName, Namespace: namespace.Name}, existingRoleBinding); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get the rolebinding associated with %s : %s", common.ArgoCDServerComponent, err)
		}
	}
	if existingRoleBinding.Name != "" {
		argoutil.LogResourceDeletion(log, existingRoleBinding, "cleaning up unmanaged source namespace")
		if err := r.Client.Delete(context.TODO(), existingRoleBinding); err != nil {
			return err
		}
	}
	return nil
}

func isProxyCluster() bool {
	cfg, err := config.GetConfig()
	if err != nil {
		log.Error(err, "failed to get k8s config")
	}

	// Initialize config client.
	configClient, err := configv1client.NewForConfig(cfg)
	if err != nil {
		log.Error(err, "failed to initialize openshift config client")
		return false
	}

	proxy, err := configClient.Proxies().Get(context.TODO(), "cluster", metav1.GetOptions{})
	if err != nil {
		log.Error(err, "failed to get proxy configuration")
		return false
	}

	if proxy.Spec.HTTPSProxy != "" {
		log.Info("proxy configuration detected")
		return true
	}

	return false
}

func (r *ReconcileArgoCD) getOpenShiftAPIURL() string {
	k8s := r.K8sClient

	cm, err := k8s.CoreV1().ConfigMaps("openshift-console").Get(context.TODO(), "console-config", metav1.GetOptions{})
	if err != nil {
		log.Error(err, "")
	}

	var cf string
	if v, ok := cm.Data["console-config.yaml"]; ok {
		cf = v
	}

	data := make(map[string]interface{})
	err = yaml.Unmarshal([]byte(cf), data)
	if err != nil {
		log.Error(err, "")
	}

	var apiURL interface{}
	var out string
	if c, ok := data["clusterInfo"]; ok {
		ci, _ := c.(map[interface{}]interface{})

		apiURL = ci["masterPublicURL"]
		out = fmt.Sprintf("%v", apiURL)
	}

	return out
}

func AddSeccompProfileForOpenShift(client client.Client, podspec *corev1.PodSpec) {
	if !IsVersionAPIAvailable() {
		return
	}
	version, err := getClusterVersion(client)
	if err != nil {
		log.Error(err, "couldn't get OpenShift version")
	}
	if version == "" || semver.Compare(fmt.Sprintf("v%s", version), "v4.10.999") > 0 {
		if podspec.SecurityContext == nil {
			podspec.SecurityContext = &corev1.PodSecurityContext{}
		}
		if podspec.SecurityContext.SeccompProfile == nil {
			podspec.SecurityContext.SeccompProfile = &corev1.SeccompProfile{}
		}
		if len(podspec.SecurityContext.SeccompProfile.Type) == 0 {
			podspec.SecurityContext.SeccompProfile.Type = corev1.SeccompProfileTypeRuntimeDefault
		}
	}
}

func IsOpenShiftCluster() bool {
	return IsVersionAPIAvailable() // This checks if the OpenShift API is available
}

// getClusterVersion returns the OpenShift Cluster version in which the operator is installed
func getClusterVersion(client client.Client) (string, error) {
	if !IsVersionAPIAvailable() {
		return "", nil
	}
	clusterVersion := &configv1.ClusterVersion{}
	err := client.Get(context.TODO(), types.NamespacedName{Name: "version"}, clusterVersion)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return clusterVersion.Status.Desired.Version, nil
}

// generateRandomBytes returns a securely generated random bytes.
func generateRandomBytes(n int) []byte {
	b := make([]byte, n)
	_, err := rand.Read(b)
	if err != nil {
		log.Error(err, "")
	}
	return b
}

// generateRandomString returns a securely generated random string.
func generateRandomString(s int) string {
	b := generateRandomBytes(s)
	return base64.URLEncoding.EncodeToString(b)
}

// contains returns true if a string is part of the given slice.
func contains(s []string, g string) bool {
	for _, a := range s {
		if a == g {
			return true
		}
	}
	return false
}

// getApplicationSetHTTPServerHost will return the host for the given ArgoCD.
func getApplicationSetHTTPServerHost(cr *argoproj.ArgoCD) (string, error) {
	host := cr.Name
	if len(cr.Spec.ApplicationSet.WebhookServer.Host) > 0 {
		hostname, err := shortenHostname(cr.Spec.ApplicationSet.WebhookServer.Host)
		if err != nil {
			return "", err
		}
		host = hostname
	}
	return host, nil
}

// UseApplicationController determines whether Application Controller resources should be created and configured or not
func UseApplicationController(name string, cr *argoproj.ArgoCD) bool {
	if name == common.ArgoCDApplicationControllerComponent && cr.Spec.Controller.Enabled != nil {
		return *cr.Spec.Controller.Enabled
	}
	return true
}

// UseRedis determines whether Redis resources should be created and configured or not
func UseRedis(name string, cr *argoproj.ArgoCD) bool {
	if name == common.ArgoCDRedisComponent && cr.Spec.Redis.Enabled != nil {
		return *cr.Spec.Redis.Enabled
	}
	return true
}

// UseServer determines whether ArgoCD Server resources should be created and configured or not
func UseServer(name string, cr *argoproj.ArgoCD) bool {
	if name == common.ArgoCDServerComponent && cr.Spec.Server.Enabled != nil {
		return *cr.Spec.Server.Enabled
	}
	return true
}

// UpdateMapValues updates the values of an existing map with the values from a source map if they differ.
// It returns true if any values were changed.
func UpdateMapValues(existing *map[string]string, source map[string]string) bool {
	changed := false
	if *existing == nil {
		*existing = make(map[string]string)
	}
	for key, value := range source {
		if (*existing)[key] != value {
			(*existing)[key] = value
			changed = true
		}
	}
	return changed
}

// updateStatusConditionOfArgoCD calls Set Condition of ArgoCD status
func updateStatusConditionOfArgoCD(ctx context.Context, condition metav1.Condition, cr *argoproj.ArgoCD, k8sClient client.Client, log logr.Logger) error {
	changed, newConditions := insertOrUpdateConditionsInSlice(condition, cr.Status.Conditions)

	if changed {
		// get the latest version of argocd instance before updating
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}, cr); err != nil {
			if apierrors.IsNotFound(err) {
				// if ArgoCD CR no longer exists, there is no status update needed, so just return.
				return nil
			}
			return err
		}

		cr.Status.Conditions = newConditions

		if err := k8sClient.Status().Update(ctx, cr); err != nil {
			log.Error(err, "unable to update RolloutManager status condition")
			return err
		}
	}
	return nil
}

// insertOrUpdateConditionsInSlice is a generic function for inserting/updating metav1.Condition into a slice of []metav1.Condition
func insertOrUpdateConditionsInSlice(newCondition metav1.Condition, existingConditions []metav1.Condition) (bool, []metav1.Condition) {

	// Check if condition with same type is already set, if Yes then check if content is same,
	// If content is not same update LastTransitionTime
	index := -1
	for i, Condition := range existingConditions {
		if Condition.Type == newCondition.Type {
			index = i
			break
		}
	}

	now := metav1.Now()

	changed := false

	if index == -1 {
		newCondition.LastTransitionTime = now
		existingConditions = append(existingConditions, newCondition)
		changed = true

	} else if existingConditions[index].Message != newCondition.Message ||
		existingConditions[index].Reason != newCondition.Reason ||
		existingConditions[index].Status != newCondition.Status {

		newCondition.LastTransitionTime = now
		existingConditions[index] = newCondition
		changed = true
	}

	return changed, existingConditions

}

// createCondition returns Condition based on input provided.
// 1. Returns Success condition if no error message is provided, all fields are default.
// 2. If Message is provided, it returns Failed condition having all default fields except Message.
func createCondition(message string) metav1.Condition {
	if message == "" {
		return metav1.Condition{
			Type:    argoproj.ArgoCDConditionType,
			Reason:  argoproj.ArgoCDConditionReasonSuccess,
			Message: "",
			Status:  metav1.ConditionTrue,
		}
	}

	return metav1.Condition{
		Type:    argoproj.ArgoCDConditionType,
		Reason:  argoproj.ArgoCDConditionReasonErrorOccurred,
		Message: message,
		Status:  metav1.ConditionFalse,
	}
}

// appendUniqueArgs appends extraArgs to cmd while ignoring any duplicate flags.
func appendUniqueArgs(cmd []string, extraArgs []string) []string {
	existing := map[string]string{}
	repeated := map[string]map[string]bool{}
	nonRepeatableFlags := map[string]bool{}
	result := []string{}

	// Helper to add flag+val to result
	add := func(flag, val string) {
		result = append(result, flag)
		if val != "" {
			result = append(result, val)
		}
	}

	// Process original cmd and treat its flags as non-repeatable
	for i := 0; i < len(cmd); i++ {
		arg := cmd[i]
		if strings.HasPrefix(arg, "--") {
			val := ""
			if i+1 < len(cmd) && !strings.HasPrefix(cmd[i+1], "--") {
				val = cmd[i+1]
				i++
			}
			if repeated[arg] == nil {
				repeated[arg] = map[string]bool{}
			}
			repeated[arg][val] = true
			existing[arg] = val
			nonRepeatableFlags[arg] = true // flags from cmd are non-repeatable
			add(arg, val)
		} else {
			result = append(result, arg)
		}
	}

	// Process extraArgs
	for i := 0; i < len(extraArgs); i++ {
		arg := extraArgs[i]
		if strings.HasPrefix(arg, "--") {
			val := ""
			if i+1 < len(extraArgs) && !strings.HasPrefix(extraArgs[i+1], "--") {
				val = extraArgs[i+1]
				i++
			}

			// Skip if this flag+val combo already exists
			if repeated[arg] != nil && repeated[arg][val] {
				continue
			}

			if nonRepeatableFlags[arg] {
				// Remove the existing non-repeatable flag (and its value)
				newResult := []string{}
				skipNext := false
				for j := 0; j < len(result); j++ {
					if skipNext {
						skipNext = false
						continue
					}
					if result[j] == arg {
						if j+1 < len(result) && !strings.HasPrefix(result[j+1], "--") {
							skipNext = true
						}
						continue
					}
					newResult = append(newResult, result[j])
				}
				result = newResult

				// Replace with new value
				repeated[arg] = map[string]bool{val: true}
				existing[arg] = val
				add(arg, val)
			} else {
				// Allow repeated if not seen before
				if repeated[arg] == nil {
					repeated[arg] = map[string]bool{}
				}
				repeated[arg][val] = true
				add(arg, val)
			}
		} else {
			result = append(result, arg)
		}
	}

	return result
}

// reconcileArgoCDAgent will reconcile all ArgoCD Agent resources.
func (r *ReconcileArgoCD) reconcileArgoCDAgent(cr *argoproj.ArgoCD) error {
	compName := "principal"
	log.Info("reconciling ArgoCD Agent resources")

	log.Info("reconciling ArgoCD Agent servie account")
	var sa *corev1.ServiceAccount
	var err error

	if sa, err = argocdagent.ReconcilePrincipalServiceAccount(r.Client, compName, cr, r.Scheme); err != nil {
		return err
	}

	log.Info("reconciling ArgoCD Agent role")
	if _, err := argocdagent.ReconcilePrincipalRole(r.Client, compName, cr, r.Scheme); err != nil {
		return err
	}

	log.Info("reconciling ArgoCD Agent cluster role")
	if _, err := argocdagent.ReconcilePrincipalClusterRoles(r.Client, compName, cr, r.Scheme); err != nil {
		return err
	}

	log.Info("reconciling ArgoCD Agent role binding")
	if err := argocdagent.ReconcilePrincipalRoleBinding(r.Client, compName, sa, cr, r.Scheme); err != nil {
		return err
	}

	log.Info("reconciling ArgoCD Agent cluster role binding")
	if err := argocdagent.ReconcilePrincipalClusterRoleBinding(r.Client, compName, sa, cr, r.Scheme); err != nil {
		return err
	}

	log.Info("reconciling ArgoCD Agent configmap")
	if err := argocdagent.ReconcilePrincipalConfigMap(r.Client, compName, cr, r.Scheme); err != nil {
		return err
	}

	log.Info("reconciling ArgoCD Agent service")
	if err := argocdagent.ReconcilePrincipalService(r.Client, compName, cr, r.Scheme); err != nil {
		return err
	}

	log.Info("reconciling ArgoCD Agent metrics service")
	if err := argocdagent.ReconcilePrincipalMetricsService(r.Client, compName, cr, r.Scheme); err != nil {
		return err
	}

	log.Info("reconciling ArgoCD Agent redis proxy service")
	if err := argocdagent.ReconcilePrincipalRedisProxyService(r.Client, compName, cr, r.Scheme); err != nil {
		return err
	}

	log.Info("reconciling ArgoCD Agent deployment")
	if err := argocdagent.ReconcilePrincipalDeployment(r.Client, compName, sa.Name, cr, r.Scheme); err != nil {
		return err
	}

	return nil
}

func (r *ReconcileArgoCD) namespaceManagementFilterPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNSMgmt, okOld := e.ObjectOld.(*argoproj.NamespaceManagement)
			newNSMgmt, okNew := e.ObjectNew.(*argoproj.NamespaceManagement)

			if !okOld || !okNew {
				return false
			}

			k8sClient := r.K8sClient
			return r.handleNamespaceManagementUpdate(oldNSMgmt, newNSMgmt, k8sClient)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			// If namespaceManagement CR is deleted, then delete RBACs for the namespace that was present in the NamespaceManagement CR.
			nsMgmt, ok := e.Object.(*argoproj.NamespaceManagement)
			if !ok {
				return false
			}
			k8sClient := r.K8sClient
			return r.handleNamespaceManagementDelete(nsMgmt, k8sClient)
		},
	}
}

func (r *ReconcileArgoCD) argoCDNamespaceManagementFilterPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			valNew, ok := e.ObjectNew.(*argoproj.ArgoCD)
			if !ok {
				return false
			}
			valOld, ok := e.ObjectOld.(*argoproj.ArgoCD)
			if !ok {
				return false
			}

			k8sClient := r.K8sClient
			return r.handleArgoCDNamespaceManagementUpdate(valNew, valOld, k8sClient)
		},
	}
}

func (r *ReconcileArgoCD) handleArgoCDNamespaceManagementUpdate(valNew, valOld *argoproj.ArgoCD, k8sClient kubernetes.Interface) bool {
	if !reflect.DeepEqual(valOld.Spec.NamespaceManagement, valNew.Spec.NamespaceManagement) {
		// Fetch all namespaces from the cluster
		namespaceList := &corev1.NamespaceList{}
		if err := r.List(context.TODO(), namespaceList); err != nil {
			log.Error(err, "Failed to list namespaces for NamespaceManagement update")
			// Return false as we cannot proceed without namespace info
			return false
		}

		// Extract all namespace names into a slice
		var allNamespaces []string
		for _, ns := range namespaceList.Items {
			allNamespaces = append(allNamespaces, ns.Name)
		}

		namespacesToDelete := getNamespacesToDelete(valOld.Spec.NamespaceManagement, valNew.Spec.NamespaceManagement, allNamespaces)

		if len(namespacesToDelete) > 0 {
			for _, nsEntry := range namespacesToDelete {
				ns := &corev1.Namespace{}
				if err := r.Get(context.TODO(), types.NamespacedName{Name: nsEntry}, ns); err != nil {
					continue // Could not fetch namespace; skip cleanup
				}

				// Skip cleanup if managed-by label exists
				if labelVal, labelExists := ns.Labels[common.ArgoCDManagedByLabel]; labelExists && labelVal == valOld.Namespace {
					log.Info(fmt.Sprintf("Skipping cleanup for namespace %s because it's still labeled as managed by this ArgoCD instance", nsEntry))
					continue
				}

				log.Info(fmt.Sprintf("Cleaning up RBACs for namespace %s", nsEntry))
				if err := cleanupRBACsForNamespaceManagement(valOld.Namespace, nsEntry, k8sClient); err != nil {
					return false
				}
			}
		}
	}
	return true
}

func (r *ReconcileArgoCD) handleNamespaceManagementUpdate(oldNSMgmt, newNSMgmt *argoproj.NamespaceManagement, k8sClient kubernetes.Interface) bool {
	ns := &corev1.Namespace{}
	if err := r.Get(context.TODO(), types.NamespacedName{Name: oldNSMgmt.Spec.ManagedBy}, ns); err != nil {
		return false
	}

	// Skip Update if managed-by label exists and matches .spec.managedBy
	if labelVal, labelExists := ns.Labels[common.ArgoCDManagedByLabel]; labelExists && labelVal == oldNSMgmt.Spec.ManagedBy {
		log.Info(fmt.Sprintf("Namespace %s still managed by same ArgoCD instance via label, skipping update cleanup", oldNSMgmt.Namespace))
		return false
	}

	// If `.spec.managedBy` changes, trigger reconciliation
	if oldNSMgmt.Spec.ManagedBy != newNSMgmt.Spec.ManagedBy {
		if err := cleanupRBACsForNamespaceManagement(oldNSMgmt.Spec.ManagedBy, oldNSMgmt.Namespace, k8sClient); err != nil {
			return false
		}
		return true
	}
	return false
}

func (r *ReconcileArgoCD) handleNamespaceManagementDelete(nsMgmt *argoproj.NamespaceManagement, k8sClient kubernetes.Interface) bool {
	ns := &corev1.Namespace{}
	if err := r.Get(context.TODO(), types.NamespacedName{Name: nsMgmt.Spec.ManagedBy}, ns); err != nil {
		return false
	}

	// Skip cleanup if managed-by label exists and matches .spec.managedBy
	if labelVal, labelExists := ns.Labels[common.ArgoCDManagedByLabel]; labelExists && labelVal == nsMgmt.Spec.ManagedBy {
		log.Info(fmt.Sprintf("Namespace %s still managed by same ArgoCD instance via label, skipping delete cleanup", nsMgmt.Namespace))
		return false
	}

	// Retrieve the ArgoCD instance that was managing this namespace
	argocdNamespace := nsMgmt.Spec.ManagedBy
	if argocdNamespace == "" {
		// If .spec.managedBy is not set, there is no ArgoCD instance managing this namespace, so no cleanup is needed
		log.Info("No ArgoCD CR specified in .spec.managedBy, skipping cleanup")
		return false
	}

	if err := cleanupRBACsForNamespaceManagement(argocdNamespace, nsMgmt.Namespace, k8sClient); err != nil {
		return false
	}
	return false
}

func cleanupRBACsForNamespaceManagement(argocdNamespace, nms string, k8sClient kubernetes.Interface) error {
	if err := deleteRBACsForNamespace(nms, k8sClient); err != nil {
		log.Error(err, fmt.Sprintf("failed to delete RBACs for namespace: %s", nms))
		return err
	} else {
		log.Info(fmt.Sprintf("Successfully removed the RBACs for namespace: %s", nms))
	}
	if err := deleteManagedNamespaceFromClusterSecret(argocdNamespace, nms, k8sClient); err != nil {
		log.Error(err, fmt.Sprintf("unable to delete namespace %s from cluster secret", nms))
		return err
	}
	return nil
}

// updateStatusConditionOfArgoCD calls Set Condition of NamespaceManagement status
func updateStatusConditionOfNamespaceManagement(ctx context.Context, condition metav1.Condition, cr *argoproj.NamespaceManagement, k8sClient client.Client, log logr.Logger) error {
	changed, newConditions := insertOrUpdateConditionsInSlice(condition, cr.Status.Conditions)

	if changed {
		// get the latest version of namespacemanagement before updating
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: cr.Name, Namespace: cr.Namespace}, cr); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		cr.Status.Conditions = newConditions
		if err := k8sClient.Status().Update(ctx, cr); err != nil {
			log.Error(err, "unable to update NamespaceManagement status condition")
			return err
		}
	}
	return nil
}

// getNamespacesToDelete determines which namespaces were removed or had allowManagedBy changed.
// oldList, newList contain patterns in ns.Name, but allNamespaces contains real namespace names.
func getNamespacesToDelete(oldList, newList []argoproj.ManagedNamespaces, allNamespaces []string) []string {
	// expandNsPatterns expands namespace patterns into actual namespaces using glob match.
	// Example: pattern "team-*" will match ["team-123", "team-567", "team-dev", "team-prod"]
	expandNsPatterns := func(list []argoproj.ManagedNamespaces) map[string]bool {
		result := make(map[string]bool)
		for _, patternEntry := range list {
			for _, ns := range allNamespaces {
				if glob.MatchStringInList([]string{patternEntry.Name}, ns, glob.GLOB) {
					result[ns] = patternEntry.AllowManagedBy
				}
			}
		}
		return result
	}

	oldExpanded := expandNsPatterns(oldList)
	newExpanded := expandNsPatterns(newList)

	var namespacesToDelete []string
	for ns, oldAllowed := range oldExpanded {
		newAllowed, exists := newExpanded[ns]
		if !exists || newAllowed != oldAllowed {
			namespacesToDelete = append(namespacesToDelete, ns)
		}
	}
	return namespacesToDelete
}
