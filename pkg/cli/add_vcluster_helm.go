package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/loft-sh/log"
	"github.com/loft-sh/log/survey"
	"github.com/loft-sh/vcluster/pkg/cli/find"
	"github.com/loft-sh/vcluster/pkg/cli/flags"
	"github.com/loft-sh/vcluster/pkg/lifecycle"
	"github.com/loft-sh/vcluster/pkg/platform"
	"github.com/loft-sh/vcluster/pkg/platform/clihelper"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

type AddVClusterOptions struct {
	Project                  string
	ImportName               string
	Restart                  bool
	Insecure                 bool
	AccessKey                string
	Host                     string
	CertificateAuthorityData []byte
	All                      bool
}

func AddVClusterHelm(
	ctx context.Context,
	options *AddVClusterOptions,
	globalFlags *flags.GlobalFlags,
	vClusterName string,
	log log.Logger,
) error {
	var vClusters []find.VCluster
	if options.All {
		log.Debugf("add vcluster called with --all flag")
		kubeClientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{
				CurrentContext: globalFlags.Context,
			})
		hostClusterRestConfig, err := kubeClientConfig.ClientConfig()
		if err != nil {
			return err
		}
		hostKubeClient, err := kubernetes.NewForConfig(hostClusterRestConfig)
		if err != nil {
			return err
		}
		namespaces, err := hostKubeClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		log.Debugf("looking for vclusters in %d namespaces", len(namespaces.Items))
		for _, ns := range namespaces.Items {
			log.Infof("looking for a vcluster in %s namespace", ns.GetName())
			vClustersInNamespace, err := find.ListVClusters(ctx, globalFlags.Context, "", ns.GetName(), log)
			if err != nil {
				return err
			}
			if len(vClustersInNamespace) == 0 {
				log.Infof("no vClusters found in context %s and namespace %s", globalFlags.Context, ns.GetName())
				continue
			}
			vClusters = append(vClusters, vClustersInNamespace...)
		}
	} else {
		// check if vCluster exists
		vCluster, err := find.GetVCluster(ctx, globalFlags.Context, vClusterName, globalFlags.Namespace, log)
		if err != nil {
			return err
		}
		vClusters = append(vClusters, *vCluster)
	}

	if len(vClusters) == 0 {
		return nil
	}

	restConfig, err := vClusters[0].ClientFactory.ClientConfig()
	if err != nil {
		return err
	}

	// create kube client
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return err
	}
	addErr := &VClusterAddError{}
	log.Debugf("trying to add %d vClusters to platform", len(vClusters))
	for _, vCluster := range vClusters {
		vCluster := vCluster
		log.Infof("adding %s vCluster to platform", vCluster.Name)
		addErr.addErr(vCluster.Name, addVClusterHelm(ctx, options, globalFlags, vCluster.Name, &vCluster, kubeClient, log))
	}

	return addErr.CombinedError()
}

type VClusterAddError struct {
	errs []error
}

func (vce *VClusterAddError) CombinedError() error {
	if len(vce.errs) == 0 {
		return nil
	} else if len(vce.errs) == 1 {
		return vce.errs[0]
	}
	errMsg := strings.Builder{}
	for _, err := range vce.errs {
		_, _ = errMsg.WriteString(err.Error() + "|")
	}
	return errors.New(errMsg.String())
}

func (vce *VClusterAddError) addErr(vClusterName string, err error) {
	if err == nil {
		return
	}
	vce.errs = append(vce.errs, fmt.Errorf("cannot add vcluster %s: %w", vClusterName, err))
}

func addVClusterHelm(
	ctx context.Context,
	options *AddVClusterOptions,
	globalFlags *flags.GlobalFlags,
	vClusterName string,
	vCluster *find.VCluster,
	kubeClient *kubernetes.Clientset,
	log log.Logger,
) error {
	snoozed := false
	// If the vCluster was paused with the helm driver, adding it to the platform will only create the secret for registration
	// which leads to confusing behavior for the user since they won't see the cluster in the platform UI until it is resumed.
	if lifecycle.IsPaused(vCluster) {
		log.Infof("vCluster %s is currently sleeping. It will not be added to the platform until it wakes again.", vCluster.Name)

		snoozeConfirmation := "No. Leave it sleeping. (It will be added automatically on next wakeup)"
		answer, err := log.Question(&survey.QuestionOptions{
			Question:     fmt.Sprintf("Would you like to wake vCluster %s now to add immediately?", vCluster.Name),
			DefaultValue: snoozeConfirmation,
			Options: []string{
				snoozeConfirmation,
				"Yes. Wake and add now.",
			},
		})
		if err != nil {
			return fmt.Errorf("failed to capture your response %w", err)
		}

		if snoozed = answer == snoozeConfirmation; !snoozed {
			if err = ResumeHelm(ctx, globalFlags, vClusterName, log); err != nil {
				return fmt.Errorf("failed to wake up vCluster %s: %w", vClusterName, err)
			}

			err = wait.PollUntilContextTimeout(ctx, time.Second, clihelper.Timeout(), false, func(ctx context.Context) (done bool, err error) {
				vCluster, err = find.GetVCluster(ctx, globalFlags.Context, vClusterName, globalFlags.Namespace, log)
				if err != nil {
					return false, err
				}

				return !lifecycle.IsPaused(vCluster), nil
			})

			if err != nil {
				return fmt.Errorf("error waiting for vCluster to wake up %w", err)
			}
		}
	}

	// apply platform secret
	err := platform.ApplyPlatformSecret(
		ctx,
		globalFlags.LoadedConfig(log),
		kubeClient,
		options.ImportName,
		vCluster.Namespace,
		options.Project,
		options.AccessKey,
		options.Host,
		options.Insecure,
		options.CertificateAuthorityData,
	)
	if err != nil {
		return err
	}

	// restart vCluster
	if options.Restart {
		err = lifecycle.DeletePods(ctx, kubeClient, "app=vcluster,release="+vCluster.Name, vCluster.Namespace, log)
		if err != nil {
			return fmt.Errorf("delete vcluster workloads: %w", err)
		}
	}

	if snoozed {
		log.Infof("vCluster %s/%s will be added the next time it awakes", vCluster.Namespace, vCluster.Name)
		log.Donef("Run 'vcluster wakeup --help' to learn how to wake up vCluster %s/%s to complete the add operation.", vCluster.Namespace, vCluster.Name)
	} else {
		log.Donef("Successfully added vCluster %s/%s", vCluster.Namespace, vCluster.Name)
	}
	return nil
}
