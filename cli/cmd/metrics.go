package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/linkerd/linkerd2/controller/api/util"
	"github.com/linkerd/linkerd2/pkg/k8s"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

type metricsOptions struct {
	namespace string
	pod       string
}

func newMetricsOptions() *metricsOptions {
	return &metricsOptions{
		namespace: defaultNamespace,
		pod:       "",
	}
}

func newCmdMetrics() *cobra.Command {
	options := newMetricsOptions()

	cmd := &cobra.Command{
		Use:   "metrics [flags] (RESOURCE)",
		Short: "Fetch metrics directly from Linkerd proxies",
		Long: `Fetch metrics directly from Linkerd proxies.

  This command initiates a port-forward to a given pod or set of pods, and
  queries the /metrics endpoint on the Linkerd proxies.

  The RESOURCE argument specifies the target resource to query metrics for:
  (TYPE/NAME)

  Examples:
  * cronjob/my-cronjob
  * deploy/my-deploy
  * ds/my-daemonset
  * job/my-job
  * po/mypod1
  * rc/my-replication-controller
  * sts/my-statefulset

  Valid resource types include:
  * cronjobs
  * daemonsets
  * deployments
  * jobs
  * pods
  * replicasets
  * replicationcontrollers
  * statefulsets`,
		Example: `  # Get metrics from pod-foo-bar in the default namespace.
  linkerd metrics po/pod-foo-bar

  # Get metrics from the web deployment in the emojivoto namespace.
  linkerd metrics -n emojivoto deploy/web

  # Get metrics from the linkerd-controller pod in the linkerd namespace.
  linkerd metrics -n linkerd $(
    kubectl --namespace linkerd get pod \
      --selector linkerd.io/control-plane-component=controller \
      --output name
  )`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			k8sAPI, err := k8s.NewAPI(kubeconfigPath, kubeContext, impersonate, impersonateGroup, 0)
			if err != nil {
				return err
			}

			pods, err := getPodsFor(k8sAPI, options.namespace, args[0])
			if err != nil {
				return err
			}

			results := getMetrics(k8sAPI, pods, k8s.ProxyAdminPortName, 30*time.Second, verbose)

			var buf bytes.Buffer
			for i, result := range results {
				content := fmt.Sprintf("#\n# POD %s (%d of %d)\n#\n", result.pod, i+1, len(results))
				if result.err == nil {
					content += string(result.metrics)
				} else {
					content += fmt.Sprintf("# ERROR %s\n", result.err)
				}
				buf.WriteString(content)
			}
			fmt.Printf("%s", buf.String())

			return nil
		},
	}

	cmd.PersistentFlags().StringVarP(&options.namespace, "namespace", "n", options.namespace, "Namespace of resource")

	return cmd
}

// getPodsFor takes a resource string, queries the Kubernetes API, and returns a
// list of pods belonging to that resource.
// This could move into `pkg/k8s` if becomes more generally useful.
func getPodsFor(clientset kubernetes.Interface, namespace string, resource string) ([]corev1.Pod, error) {
	// TODO: BuildResource parses a resource string (which we need), but returns
	// objects in Public API protobuf form for submission to the Public API
	// (which we don't need). Refactor this API to strictly support parsing
	// resource strings.
	res, err := util.BuildResource(namespace, resource)
	if err != nil {
		return nil, err
	}

	if res.GetName() == "" {
		return nil, errors.New("no resource name provided")
	}

	// special case if a single pod was specified
	if res.GetType() == k8s.Pod {
		pod, err := clientset.CoreV1().Pods(namespace).Get(res.GetName(), metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return []corev1.Pod{*pod}, nil
	}

	var matchLabels map[string]string
	switch res.GetType() {
	case k8s.CronJob:
		jobs, err := clientset.BatchV1().Jobs(namespace).List(metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		var pods []corev1.Pod
		for _, job := range jobs.Items {
			if isOwner(res.GetName(), job.GetOwnerReferences()) {
				jobPods, err := getPodsFor(clientset, namespace, fmt.Sprintf("%s/%s", k8s.Job, job.GetName()))
				if err != nil {
					return nil, err
				}
				pods = append(pods, jobPods...)
			}
		}
		return pods, nil

	case k8s.DaemonSet:
		ds, err := clientset.AppsV1().DaemonSets(namespace).Get(res.GetName(), metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		matchLabels = ds.Spec.Selector.MatchLabels

	case k8s.Deployment:
		deployment, err := clientset.AppsV1().Deployments(namespace).Get(res.GetName(), metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		matchLabels = deployment.Spec.Selector.MatchLabels

	case k8s.Job:
		job, err := clientset.BatchV1().Jobs(namespace).Get(res.GetName(), metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		matchLabels = job.Spec.Selector.MatchLabels

	case k8s.ReplicaSet:
		rs, err := clientset.AppsV1().ReplicaSets(namespace).Get(res.GetName(), metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		matchLabels = rs.Spec.Selector.MatchLabels

	case k8s.ReplicationController:
		rc, err := clientset.CoreV1().ReplicationControllers(namespace).Get(res.GetName(), metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		matchLabels = rc.Spec.Selector

	case k8s.StatefulSet:
		ss, err := clientset.AppsV1().StatefulSets(namespace).Get(res.GetName(), metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		matchLabels = ss.Spec.Selector.MatchLabels

	default:
		return nil, fmt.Errorf("unsupported resource type: %s", res.GetType())
	}

	podList, err := clientset.
		CoreV1().
		Pods(namespace).
		List(
			metav1.ListOptions{
				LabelSelector: labels.Set(matchLabels).AsSelector().String(),
			},
		)
	if err != nil {
		return nil, err
	}

	return podList.Items, nil
}

func isOwner(resourceName string, ownerRefs []metav1.OwnerReference) bool {
	for _, or := range ownerRefs {
		if resourceName == or.Name {
			return true
		}
	}
	return false
}
