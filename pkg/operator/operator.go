package operator

import (
	"context"
	"fmt"
	"time"

	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	routemetricscontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller/route-metrics"
	errorpageconfigmapcontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller/sync-http-error-code-configmap"
	"github.com/openshift/library-go/pkg/operator/onepodpernodeccontroller"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	logf "github.com/openshift/cluster-ingress-operator/pkg/log"
	"github.com/openshift/cluster-ingress-operator/pkg/manifests"
	operatorclient "github.com/openshift/cluster-ingress-operator/pkg/operator/client"
	operatorconfig "github.com/openshift/cluster-ingress-operator/pkg/operator/config"
	operatorcontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller"
	canarycontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller/canary"
	certcontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller/certificate"
	certpublishercontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller/certificate-publisher"
	clientcacontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller/clientca-configmap"
	configurableroutecontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller/configurable-route"
	crlcontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller/crl"
	dnscontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller/dns"
	ingress "github.com/openshift/cluster-ingress-operator/pkg/operator/controller/ingress"
	ingresscontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller/ingress"
	ingressclasscontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller/ingressclass"
	statuscontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller/status"
	"github.com/openshift/library-go/pkg/operator/events"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

var (
	log = logf.Logger.WithName("init")
)

func init() {
	// Setup controller-runtime logging
	logf.SetRuntimeLogger(log)
}

// Operator is the scaffolding for the ingress operator. It sets up dependencies
// and defines the topology of the operator and its managed components, wiring
// them together. Operator knows what namespace the operator lives in, and what
// specific resoure types in other namespaces should produce operator events.
type Operator struct {
	client client.Client

	manager manager.Manager

	namespace string
}

// New creates (but does not start) a new operator from configuration.
func New(config operatorconfig.Config, kubeConfig *rest.Config) (*Operator, error) {
	scheme := operatorclient.GetScheme()
	// Set up an operator manager for the operator namespace.
	mgr, err := manager.New(kubeConfig, manager.Options{
		Namespace: config.Namespace,
		Scheme:    scheme,
		NewCache: cache.MultiNamespacedCacheBuilder([]string{
			config.Namespace,
			operatorcontroller.GlobalUserSpecifiedConfigNamespace,
			operatorcontroller.DefaultOperandNamespace,
			operatorcontroller.DefaultCanaryNamespace,
			operatorcontroller.GlobalMachineSpecifiedConfigNamespace,
			operatorcontroller.SourceConfigMapNamespace,
		}),
		// Use a non-caching client everywhere. The default split client does not
		// promise to invalidate the cache during writes (nor does it promise
		// sequential create/get coherence), and we have code which (probably
		// incorrectly) assumes a get immediately following a create/update will
		// return the updated resource. All client consumers will need audited to
		// ensure they are tolerant of stale data (or we need a cache or client that
		// makes stronger coherence guarantees).
		NewClient: func(_ cache.Cache, config *rest.Config, options client.Options, uncachedObjects ...client.Object) (client.Client, error) {
			return client.New(config, options)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create operator manager: %v", err)
	}

	// Create and register the ingress controller with the operator manager.
	if _, err := ingresscontroller.New(mgr, ingresscontroller.Config{
		Namespace:              config.Namespace,
		IngressControllerImage: config.IngressControllerImage,
	}); err != nil {
		return nil, fmt.Errorf("failed to create ingress controller: %v", err)
	}

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kube client: %w", err)
	}
	namespaceInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 24*time.Hour, informers.WithNamespace(operatorcontroller.DefaultOperandNamespace))
	// this only handles the case for the default router which is used for oauth-server, console, and other
	// platform services.  The scheduler bug will need to be fixed to correct the rest.
	// Once https://bugzilla.redhat.com/show_bug.cgi?id=2062459 is fixed, this controller can be removed.
	forcePodSpread := onepodpernodeccontroller.NewOnePodPerNodeController(
		"spread-default-router-pods",
		operatorcontroller.DefaultOperandNamespace,
		operatorcontroller.IngressControllerDeploymentPodSelector(&operatorv1.IngressController{ObjectMeta: metav1.ObjectMeta{Name: "default"}}),
		30, // minReadySeconds from deployments.apps/router-default
		events.NewKubeRecorder(kubeClient.CoreV1().Events(config.Namespace), "cluster-ingress-operator", &corev1.ObjectReference{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
			Namespace:  config.Namespace,
			Name:       "ingress-operator",
		}),
		// ingress operator appears to be wired to a namespaced resource
		v1helpers.NewFakeOperatorClient(&operatorv1.OperatorSpec{ManagementState: operatorv1.Managed}, &operatorv1.OperatorStatus{}, nil),
		kubeClient,
		namespaceInformers.Core().V1().Pods(),
	)
	go forcePodSpread.Run(context.TODO(), 1)
	go namespaceInformers.Start(config.Stop)

	// Create and register the configurable route controller with the operator manager.
	if _, err := configurableroutecontroller.New(mgr, configurableroutecontroller.Config{
		SecretNamespace: operatorcontroller.GlobalUserSpecifiedConfigNamespace,
	}, events.NewLoggingEventRecorder(configurableroutecontroller.ControllerName)); err != nil {
		return nil, fmt.Errorf("failed to create configurable-route controller: %v", err)
	}

	// Set up the status controller.
	if _, err := statuscontroller.New(mgr, statuscontroller.Config{
		Namespace:              config.Namespace,
		IngressControllerImage: config.IngressControllerImage,
		CanaryImage:            config.CanaryImage,
		OperatorReleaseVersion: config.OperatorReleaseVersion,
	}); err != nil {
		return nil, fmt.Errorf("failed to create status controller: %v", err)
	}

	// Set up the certificate controller
	if _, err := certcontroller.New(mgr, config.Namespace); err != nil {
		return nil, fmt.Errorf("failed to create cacert controller: %v", err)
	}

	// Set up the error-page configmap controller.
	if _, err := errorpageconfigmapcontroller.New(mgr, errorpageconfigmapcontroller.Config{
		OperatorNamespace: config.Namespace,
		ConfigNamespace:   operatorcontroller.GlobalUserSpecifiedConfigNamespace,
		OperandNamespace:  operatorcontroller.DefaultOperandNamespace,
	}); err != nil {
		return nil, fmt.Errorf("failed to create error-page configmap controller: %w", err)
	}

	// Set up the certificate-publisher controller
	if _, err := certpublishercontroller.New(mgr, config.Namespace, operatorcontroller.DefaultOperandNamespace); err != nil {
		return nil, fmt.Errorf("failed to create certificate-publisher controller: %v", err)
	}

	// Set up the client CA configmap controller
	if _, err := clientcacontroller.New(mgr, clientcacontroller.Config{
		OperatorNamespace: config.Namespace,
		SourceNamespace:   operatorcontroller.GlobalUserSpecifiedConfigNamespace,
		TargetNamespace:   operatorcontroller.DefaultOperandNamespace,
	}); err != nil {
		return nil, fmt.Errorf("failed to create client CA configmap controller: %w", err)
	}

	// Set up the crl controller
	if _, err := crlcontroller.New(mgr); err != nil {
		return nil, fmt.Errorf("failed to create crl controller: %v", err)
	}

	// Set up the DNS controller
	if _, err := dnscontroller.New(mgr, dnscontroller.Config{
		Namespace:              config.Namespace,
		OperatorReleaseVersion: config.OperatorReleaseVersion,
	}); err != nil {
		return nil, fmt.Errorf("failed to create dns controller: %v", err)
	}

	// Set up the ingressclass controller.
	if _, err := ingressclasscontroller.New(mgr, ingressclasscontroller.Config{
		Namespace: config.Namespace,
	}); err != nil {
		return nil, fmt.Errorf("failed to create ingressclass controller: %w", err)
	}

	// Set up the canary controller when the config.CanaryImage is not empty
	// Canary can be disabled when running the operator locally.
	if len(config.CanaryImage) != 0 {
		if _, err := canarycontroller.New(mgr, canarycontroller.Config{
			Namespace:   config.Namespace,
			CanaryImage: config.CanaryImage,
			Stop:        config.Stop,
		}); err != nil {
			return nil, fmt.Errorf("failed to create canary controller: %v", err)
		}
	}

	// Set up the route metrics controller.
	if _, err := routemetricscontroller.New(mgr, config.Namespace); err != nil {
		return nil, fmt.Errorf("failed to create route metrics controller: %w", err)
	}

	return &Operator{
		manager: mgr,
		// TODO: These are only needed for the default ingress controller stuff, which
		// should be refactored away.
		client:    mgr.GetClient(),
		namespace: config.Namespace,
	}, nil
}

// Start creates the default IngressController and then starts the operator
// synchronously until a message is received on the stop channel.
// TODO: Move the default IngressController logic elsewhere.
func (o *Operator) Start(ctx context.Context) error {

	infraConfig := &configv1.Infrastructure{}
	if err := o.client.Get(context.TODO(), types.NamespacedName{Name: "cluster"}, infraConfig); err != nil {
		return fmt.Errorf("failed fetching infrastructure config: %w", err)
	}

	if infraConfig.Status.ControlPlaneTopology == configv1.ExternalTopologyMode {
		log.Info("skipping default ingress controller creation")
	} else {
		// Periodicaly ensure the default controller exists.
		go wait.Until(func() {
			if !o.manager.GetCache().WaitForCacheSync(ctx) {
				log.Error(nil, "failed to sync cache before ensuring default ingresscontroller")
				return
			}
			ingressConfigName := operatorcontroller.IngressClusterConfigName()
			ingressConfig := &configv1.Ingress{}
			if err := o.client.Get(context.TODO(), ingressConfigName, ingressConfig); err != nil {
				log.Error(err, "failed to fetch ingress config")
				return
			}
			err := o.ensureDefaultIngressController(infraConfig, ingressConfig)
			if err != nil {
				log.Error(err, "failed to ensure default ingresscontroller")
			}
		}, 1*time.Minute, ctx.Done())

	}

	go wait.Until(o.ensureLogLevel, 1*time.Minute, ctx.Done())

	if err := o.handleSingleNode4Dot11Upgrade(); err != nil {
		log.Error(err, "failed to handle single node 4.11 upgrade logic")
	}

	errChan := make(chan error)
	go func() {
		errChan <- o.manager.Start(ctx)
	}()

	// Wait for the manager to exit or an explicit stop.
	select {
	case <-ctx.Done():
		return nil
	case err := <-errChan:
		return err
	}
}

func (o *Operator) ensureLogLevel() {
	ingressConfig := &configv1.Ingress{}
	if err := o.client.Get(context.TODO(), types.NamespacedName{Name: "cluster"}, ingressConfig); err != nil {
		log.Error(err, "failed fetching ingress config: %w")
		return
	}

	if ingressConfig != nil {
		// If OperatorLogLevel is "Normal" or unset, the default is Info. Only
		// log.V(0).Info, log.Info, and log.Error log messages will be logged
		desiredLogLevel := zap.InfoLevel
		vLevel := 0
		switch ingressConfig.Spec.OperatorLogLevel {
		case configv1.IngressOperatorLogLevelDebug:
			// log.V(1).Info messages will be logged at Debug level
			desiredLogLevel = zap.DebugLevel
			vLevel = 1
		case configv1.IngressOperatorLogLevelTrace:
			// log.V(2).Info messages will be logged at Trace level
			desiredLogLevel = zapcore.Level(-2)
			vLevel = 2
		case configv1.IngressOperatorLogLevelTraceAll:
			// log.V(3).Info messages will be logged at TraceAll level
			desiredLogLevel = zapcore.Level(-3)
			vLevel = 3
		}

		if logf.CurrentLogLevel.Level() != desiredLogLevel {
			logf.CurrentLogLevel.SetLevel(desiredLogLevel)
			log.V(vLevel).Info("Updated log level", "CurrentLogLevel", desiredLogLevel.String())
		}
	}
}

func (o *Operator) determineReplicasForDefaultIngressController(infraConfig *configv1.Infrastructure) (int32, error) {
	ingressConfig := &configv1.Ingress{}
	if err := o.client.Get(context.TODO(), types.NamespacedName{Name: "cluster"}, ingressConfig); err != nil {
		return 0, fmt.Errorf("failed fetching ingress config: %w", err)
	}

	return ingress.DetermineReplicas(ingressConfig, infraConfig), nil
}

// handleSingleNode4Dot11Upgrade sets the defaultPlacement status in the
// ingress config CR of none-platform single node clusters to "ControlPlane" if
// it's not already set. The situations in which this value is not set are in
// clusters upgraded from <4.11 to >=4.11. When that field is not set, it is
// assumed by this operator's controllers to have the value "Workers", which is
// fine for most clusters, but not a good default for none-platform single-node
// clusters as it causes issues when adding workers to them (and is why
// starting with 4.11 freshly installed none-platform have it set to
// "ControlPlane"). The purpose of this function is to then "fix" those older
// clusters so they behave more like freshly installed >=4.11 clusters. Before
// doing that we also verify that the cluster truly has only one node, as we
// wouldn't want to affect clusters that have workers. That's because it's
// likely that such clusters already have a custom ingress configuration that
// we wouldn't want to potentially mess up, even though adding workers was
// previously unsupported.
// If defaultPlacement is unset and the cluster doesn't match the criteria
// mentioned above, it is anyway given the explicit "Workers" value to ensure
// this function doesn't affect it in the future no matter what changes (e.g.
// number of nodes), and to make it more consistent with freshly installed
// >=4.11 clusters.
func (o *Operator) handleSingleNode4Dot11Upgrade() error {
	infraConfig := &configv1.Infrastructure{}
	if err := o.client.Get(context.TODO(), operatorcontroller.InfrastructureClusterConfigName(),
		infraConfig); err != nil {
		return fmt.Errorf("failed fetching infrastructure config: %w", err)
	}

	ingressConfigName := operatorcontroller.IngressClusterConfigName()
	ingressConfig := &configv1.Ingress{}
	if err := o.client.Get(context.TODO(), ingressConfigName, ingressConfig); err != nil {
		return fmt.Errorf("failed fetching ingress config: %w", err)
	}

	if ingressConfig.Status.DefaultPlacement != "" {
		return nil
	}

	desiredDefaultPlacement := configv1.DefaultPlacementWorkers

	nodes := &corev1.NodeList{}
	if err := o.client.List(context.TODO(), nodes, &client.ListOptions{}); err != nil {
		return fmt.Errorf("failed fetching cluster nodes: %w", err)
	}

	if len(nodes.Items) == 1 &&
		infraConfig.Status.ControlPlaneTopology == configv1.SingleReplicaTopologyMode &&
		infraConfig.Status.InfrastructureTopology == configv1.SingleReplicaTopologyMode &&
		infraConfig.Status.PlatformStatus.Type == configv1.NonePlatformType {
		desiredDefaultPlacement = configv1.DefaultPlacementControlPlane
	}

	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := o.client.Get(context.TODO(), ingressConfigName, ingressConfig); err != nil {
			return err
		}

		patch := client.MergeFrom(ingressConfig.DeepCopy())
		ingressConfig.Status.DefaultPlacement = desiredDefaultPlacement
		err := o.client.Status().Patch(context.TODO(), ingressConfig, patch)

		return err
	}); err != nil {
		return fmt.Errorf("unable to update ingress config %q: %w", ingressConfigName.Name, err)
	}

	log.Info("Patched ingress config defaultPlacement status", ingressConfigName.Name, desiredDefaultPlacement)

	return nil
}

// ensureDefaultIngressController creates the default ingresscontroller if it
// doesn't already exist.
func (o *Operator) ensureDefaultIngressController(infraConfig *configv1.Infrastructure, ingressConfig *configv1.Ingress) error {
	name := types.NamespacedName{Namespace: o.namespace, Name: manifests.DefaultIngressControllerName}
	ic := &operatorv1.IngressController{}
	if err := o.client.Get(context.TODO(), name, ic); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	// Set the replicas field to a non-nil value because otherwise its
	// persisted value will be nil, which causes GETs on the /scale
	// subresource to fail, which breaks the scaling client.  See also:
	// https://github.com/kubernetes/kubernetes/pull/75210
	replicas := ingress.DetermineReplicas(ingressConfig, infraConfig)

	ic = &operatorv1.IngressController{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name.Name,
			Namespace: name.Namespace,
		},
		Spec: operatorv1.IngressControllerSpec{
			Replicas: &replicas,
		},
	}
	if ingressConfig.Spec.LoadBalancer.Platform.Type == configv1.AWSPlatformType {
		if ingressConfig.Spec.LoadBalancer.Platform.AWS != nil && ingressConfig.Spec.LoadBalancer.Platform.AWS.Type == configv1.NLB {
			ic.Spec.EndpointPublishingStrategy = &operatorv1.EndpointPublishingStrategy{
				Type: operatorv1.LoadBalancerServiceStrategyType,
				LoadBalancer: &operatorv1.LoadBalancerStrategy{
					Scope: "External",
					ProviderParameters: &operatorv1.ProviderLoadBalancerParameters{
						Type: operatorv1.AWSLoadBalancerProvider,
						AWS: &operatorv1.AWSLoadBalancerParameters{
							Type: operatorv1.AWSNetworkLoadBalancer,
						},
					},
				},
			}
		}
	}

	if err := o.client.Create(context.TODO(), ic); err != nil {
		return err
	}
	log.Info("created default ingresscontroller", "namespace", ic.Namespace, "name", ic.Name)
	return nil
}
