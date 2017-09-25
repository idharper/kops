/*
Copyright 2016 The Kubernetes Authors.

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

package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/golang/glog"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kops/cmd/kops/util"
	api "k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/cloudinstances"
	"k8s.io/kops/pkg/featureflag"
	"k8s.io/kops/pkg/instancegroups"
	"k8s.io/kops/pkg/pretty"
	"k8s.io/kops/upup/pkg/fi/cloudup"
	"k8s.io/kops/upup/pkg/kutil"
	"k8s.io/kops/util/pkg/tables"
	"k8s.io/kubernetes/pkg/kubectl/cmd/templates"
	"k8s.io/kubernetes/pkg/util/i18n"
)

var (
	rollingupdate_long = pretty.LongDesc(i18n.T(`
	This command updates a kubernetes cluster to match the cloud and kops specifications.

	To perform a rolling update, you need to update the cloud resources first with the command
	` + pretty.Bash("kops update cluster") + `.

	If rolling-update does not report that the cluster needs to be rolled, you can force the cluster to be
	rolled with the force flag.  Rolling update drains and validates the cluster by default.  A cluster is
	deemed validated when all required nodes are running and all pods in the kube-system namespace are operational.
	When a node is deleted, rolling-update sleeps the interval for the node type, and then tries for the same period
	of time for the cluster to be validated.  For instance, setting --master-interval=3m causes rolling-update
	to wait for 3 minutes after a master is rolled, and another 3 minutes for the cluster to stabilize and pass
	validation.

	Note: terraform users will need to run all of the following commands from the same directory
	` + pretty.Bash("kops update cluster --target=terraform") + ` then ` + pretty.Bash("terraform plan") + ` then
	` + pretty.Bash("terraform apply") + ` prior to running ` + pretty.Bash("kops rolling-update cluster") + `.`))

	rollingupdate_example = templates.Examples(i18n.T(`
		# Preview a rolling-update.
		kops rolling-update cluster

		# Roll the currently selected kops cluster with defaults.
		# Nodes will be drained and the cluster will be validated between node replacement.
		kops rolling-update cluster --yes

		# Roll the k8s-cluster.example.com kops cluster,
		# do not fail if the cluster does not validate,
		# wait 8 min to create new node, and wait at least
		# 8 min to validate the cluster.
		kops rolling-update cluster k8s-cluster.example.com --yes \
		  --fail-on-validate-error="false" \
		  --master-interval=8m \
		  --node-interval=8m

		# Roll the k8s-cluster.example.com kops cluster,
		# do not validate the cluster because of the cloudonly flag.
	    # Force the entire cluster to roll, even if rolling update
	    # reports that the cluster does not need to be rolled.
		kops rolling-update cluster k8s-cluster.example.com --yes \
	      --cloudonly \
		  --force

		# Roll the k8s-cluster.example.com kops cluster,
		# only roll the node instancegroup,
		# use the new drain an validate functionality.
		kops rolling-update cluster k8s-cluster.example.com --yes \
		  --fail-on-validate-error="false" \
		  --node-interval 8m \
		  --instance-group nodes
		`))

	rollingupdate_short = i18n.T(`Rolling update a cluster.`)
)

// RollingUpdateOptions is the command Object for a Rolling Update.
type RollingUpdateOptions struct {
	Yes       bool
	Force     bool
	CloudOnly bool

	// The following two variables are when kops is validating a cluster
	// during a rolling update.

	// FailOnDrainError fail rolling-update if drain errors.
	FailOnDrainError bool

	// FailOnValidate fail the cluster rolling-update when the cluster
	// does not validate, after a validation period.
	FailOnValidate bool

	DrainInterval time.Duration

	MasterInterval  time.Duration
	NodeInterval    time.Duration
	BastionInterval time.Duration

	ClusterName string

	// InstanceGroups is the list of instance groups to rolling-update;
	// if not specified, all instance groups will be updated
	InstanceGroups []string
}

func (o *RollingUpdateOptions) InitDefaults() {
	o.Yes = false
	o.Force = false
	o.CloudOnly = false
	o.FailOnDrainError = false
	o.FailOnValidate = true

	o.MasterInterval = 5 * time.Minute
	o.NodeInterval = 4 * time.Minute
	o.BastionInterval = 5 * time.Minute

	o.DrainInterval = 90 * time.Second

}

func NewCmdRollingUpdateCluster(f *util.Factory, out io.Writer) *cobra.Command {

	var options RollingUpdateOptions
	options.InitDefaults()

	cmd := &cobra.Command{
		Use:     "cluster",
		Short:   rollingupdate_short,
		Long:    rollingupdate_long,
		Example: rollingupdate_example,
	}

	cmd.Flags().BoolVar(&options.Yes, "yes", options.Yes, "perform rolling update without confirmation")
	cmd.Flags().BoolVar(&options.Force, "force", options.Force, "Force rolling update, even if no changes")
	cmd.Flags().BoolVar(&options.CloudOnly, "cloudonly", options.CloudOnly, "Perform rolling update without confirming progress with k8s")

	cmd.Flags().DurationVar(&options.MasterInterval, "master-interval", options.MasterInterval, "Time to wait between restarting masters")
	cmd.Flags().DurationVar(&options.NodeInterval, "node-interval", options.NodeInterval, "Time to wait between restarting nodes")
	cmd.Flags().DurationVar(&options.BastionInterval, "bastion-interval", options.BastionInterval, "Time to wait between restarting bastions")
	cmd.Flags().StringSliceVar(&options.InstanceGroups, "instance-group", options.InstanceGroups, "List of instance groups to update (defaults to all if not specified)")

	if featureflag.DrainAndValidateRollingUpdate.Enabled() {
		cmd.Flags().BoolVar(&options.FailOnDrainError, "fail-on-drain-error", true, "The rolling-update will fail if draining a node fails.")
		cmd.Flags().BoolVar(&options.FailOnValidate, "fail-on-validate-error", true, "The rolling-update will fail if the cluster fails to validate.")
	}

	cmd.Run = func(cmd *cobra.Command, args []string) {
		err := rootCommand.ProcessArgs(args)
		if err != nil {
			exitWithError(err)
			return
		}

		clusterName := rootCommand.ClusterName()
		if clusterName == "" {
			exitWithError(fmt.Errorf("--name is required"))
			return
		}

		options.ClusterName = clusterName

		err = RunRollingUpdateCluster(f, os.Stdout, &options)
		if err != nil {
			exitWithError(err)
			return
		}

	}

	return cmd
}

func RunRollingUpdateCluster(f *util.Factory, out io.Writer, options *RollingUpdateOptions) error {

	clientset, err := f.Clientset()
	if err != nil {
		return err
	}

	cluster, err := GetCluster(f, options.ClusterName)
	if err != nil {
		return err
	}

	contextName := cluster.ObjectMeta.Name
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: contextName}).ClientConfig()
	if err != nil {
		return fmt.Errorf("cannot load kubecfg settings for %q: %v", contextName, err)
	}

	var nodes []v1.Node
	var k8sClient kubernetes.Interface
	if !options.CloudOnly {
		k8sClient, err = kubernetes.NewForConfig(config)
		if err != nil {
			return fmt.Errorf("cannot build kube client for %q: %v", contextName, err)
		}

		nodeList, err := k8sClient.CoreV1().Nodes().List(metav1.ListOptions{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Unable to reach the kubernetes API.\n")
			fmt.Fprintf(os.Stderr, "Use --cloudonly to do a rolling-update without confirming progress with the k8s API\n\n")
			return fmt.Errorf("error listing nodes in cluster: %v", err)
		}

		if nodeList != nil {
			nodes = nodeList.Items
		}
	}

	list, err := clientset.InstanceGroupsFor(cluster).List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	var instanceGroups []*api.InstanceGroup
	for i := range list.Items {
		instanceGroups = append(instanceGroups, &list.Items[i])
	}

	warnUnmatched := true

	if len(options.InstanceGroups) != 0 {
		var filtered []*api.InstanceGroup

		for _, instanceGroupName := range options.InstanceGroups {
			var found *api.InstanceGroup
			for _, ig := range instanceGroups {
				if ig.ObjectMeta.Name == instanceGroupName {
					found = ig
					break
				}
			}
			if found == nil {
				return fmt.Errorf("InstanceGroup %q not found", instanceGroupName)
			}

			filtered = append(filtered, found)
		}

		instanceGroups = filtered

		// Don't warn if we find more ASGs than IGs
		warnUnmatched = false
	}

	cloud, err := cloudup.BuildCloud(cluster)
	if err != nil {
		return err
	}

	groups, err := cloud.GetCloudGroups(cluster, instanceGroups, warnUnmatched, nodes)
	if err != nil {
		return err
	}

	{
		t := &tables.Table{}
		t.AddColumn("NAME", func(r *cloudinstances.CloudInstanceGroup) string {
			return r.InstanceGroup.ObjectMeta.Name
		})
		t.AddColumn("STATUS", func(r *cloudinstances.CloudInstanceGroup) string {
			return r.Status
		})
		t.AddColumn("NEEDUPDATE", func(r *cloudinstances.CloudInstanceGroup) string {
			return strconv.Itoa(len(r.NeedUpdate))
		})
		t.AddColumn("READY", func(r *cloudinstances.CloudInstanceGroup) string {
			return strconv.Itoa(len(r.Ready))
		})
		t.AddColumn("MIN", func(r *cloudinstances.CloudInstanceGroup) string {
			return strconv.Itoa(r.MinSize)
		})
		t.AddColumn("MAX", func(r *cloudinstances.CloudInstanceGroup) string {
			return strconv.Itoa(r.MaxSize)
		})
		t.AddColumn("NODES", func(r *cloudinstances.CloudInstanceGroup) string {
			var nodes []*v1.Node
			for _, i := range r.Ready {
				if i.Node != nil {
					nodes = append(nodes, i.Node)
				}
			}
			for _, i := range r.NeedUpdate {
				if i.Node != nil {
					nodes = append(nodes, i.Node)
				}
			}
			return strconv.Itoa(len(nodes))
		})
		var l []*cloudinstances.CloudInstanceGroup
		for _, v := range groups {
			l = append(l, v)
		}

		columns := []string{"NAME", "STATUS", "NEEDUPDATE", "READY", "MIN", "MAX"}
		if !options.CloudOnly {
			columns = append(columns, "NODES")
		}
		err := t.Render(l, out, columns...)
		if err != nil {
			return err
		}
	}

	needUpdate := false
	for _, group := range groups {
		if len(group.NeedUpdate) != 0 {
			needUpdate = true
		}
	}

	if !needUpdate && !options.Force {
		fmt.Printf("\nNo rolling-update required.\n")
		return nil
	}

	if !options.Yes {
		fmt.Printf("\nMust specify --yes to rolling-update.\n")
		return nil
	}

	if featureflag.DrainAndValidateRollingUpdate.Enabled() {
		glog.V(2).Infof("Rolling update with drain and validate enabled.")
	}
	d := &instancegroups.RollingUpdateCluster{
		MasterInterval:   options.MasterInterval,
		NodeInterval:     options.NodeInterval,
		Force:            options.Force,
		Cloud:            cloud,
		K8sClient:        k8sClient,
		ClientConfig:     kutil.NewClientConfig(config, "kube-system"),
		FailOnDrainError: options.FailOnDrainError,
		FailOnValidate:   options.FailOnValidate,
		CloudOnly:        options.CloudOnly,
		ClusterName:      options.ClusterName,
		DrainInterval:    options.DrainInterval,
	}
	return d.RollingUpdate(groups, list)
}
