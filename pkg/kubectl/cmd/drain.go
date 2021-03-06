/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package cmd

import (
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/spf13/cobra"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/meta"
	"k8s.io/kubernetes/pkg/controller"
	// "k8s.io/kubernetes/pkg/api/unversioned"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/fields"
	cmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/kubectl/resource"
	"k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/runtime"
)

type DrainOptions struct {
	client             *client.Client
	factory            *cmdutil.Factory
	Force              bool
	GracePeriodSeconds int
	mapper             meta.RESTMapper
	nodeInfo           *resource.Info
	out                io.Writer
	typer              runtime.ObjectTyper
}

const (
	cordon_long = `Mark node as unschedulable.
`
	cordon_example = `# Mark node "foo" as unschedulable.
$ kubectl cordon foo
`
)

func NewCmdCordon(f *cmdutil.Factory, out io.Writer) *cobra.Command {
	options := &DrainOptions{factory: f, out: out}

	return &cobra.Command{
		Use:     "cordon NODE",
		Short:   "Mark node as unschedulable",
		Long:    cordon_long,
		Example: cordon_example,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(options.SetupDrain(cmd, args))
			cmdutil.CheckErr(options.RunCordonOrUncordon(true))
		},
	}
}

const (
	uncordon_long = `Mark node as schedulable.
`
	uncordon_example = `# Mark node "foo" as schedulable.
$ kubectl uncordon foo
`
)

func NewCmdUncordon(f *cmdutil.Factory, out io.Writer) *cobra.Command {
	options := &DrainOptions{factory: f, out: out}

	return &cobra.Command{
		Use:     "uncordon NODE",
		Short:   "Mark node as schedulable",
		Long:    uncordon_long,
		Example: uncordon_example,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(options.SetupDrain(cmd, args))
			cmdutil.CheckErr(options.RunCordonOrUncordon(false))
		},
	}
}

const (
	drain_long = `Drain node in preparation for maintenance.

The given node will be marked unschedulable to prevent new pods from arriving.
Then drain deletes all pods except mirror pods (which cannot be deleted through
the API server).  If there are any pods that are neither mirror pods nor
managed by a ReplicationController, Job, or DaemonSet, then drain will not
delete any pods unless you use --force.

When you are ready to put the node back into service, use kubectl uncordon, which
will make the node schedulable again.
`
	drain_example = `# Drain node "foo", even if there are pods not managed by a ReplicationController, Job, or DaemonSet on it.
$ kubectl drain foo --force

# As above, but abort if there are pods not managed by a ReplicationController, Job, or DaemonSet, and use a grace period of 15 minutes.
$ kubectl drain foo --grace-period=900
`
)

func NewCmdDrain(f *cmdutil.Factory, out io.Writer) *cobra.Command {
	options := &DrainOptions{factory: f, out: out}

	cmd := &cobra.Command{
		Use:     "drain NODE",
		Short:   "Drain node in preparation for maintenance",
		Long:    drain_long,
		Example: drain_example,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(options.SetupDrain(cmd, args))
			cmdutil.CheckErr(options.RunDrain())
		},
	}
	cmd.Flags().BoolVar(&options.Force, "force", false, "Continue even if there are pods not managed by a ReplicationController, Job, or DaemonSet.")
	cmd.Flags().IntVar(&options.GracePeriodSeconds, "grace-period", -1, "Period of time in seconds given to each pod to terminate gracefully. If negative, the default value specified in the pod will be used.")
	return cmd
}

// SetupDrain populates some fields from the factory, grabs command line
// arguments and looks up the node using Builder
func (o *DrainOptions) SetupDrain(cmd *cobra.Command, args []string) error {
	var err error
	if len(args) != 1 {
		return cmdutil.UsageError(cmd, fmt.Sprintf("USAGE: %s [flags]", cmd.Use))
	}

	if o.client, err = o.factory.Client(); err != nil {
		return err
	}

	o.mapper, o.typer = o.factory.Object()

	cmdNamespace, _, err := o.factory.DefaultNamespace()
	if err != nil {
		return err
	}

	r := o.factory.NewBuilder().
		NamespaceParam(cmdNamespace).DefaultNamespace().
		ResourceNames("node", args[0]).
		Do()

	if err = r.Err(); err != nil {
		return err
	}

	return r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}
		o.nodeInfo = info
		return nil
	})
}

// RunDrain runs the 'drain' command
func (o *DrainOptions) RunDrain() error {
	if err := o.RunCordonOrUncordon(true); err != nil {
		return err
	}

	pods, err := o.getPodsForDeletion()
	if err != nil {
		return err
	}

	if err = o.deletePods(pods); err != nil {
		return err
	}
	cmdutil.PrintSuccess(o.mapper, false, o.out, "node", o.nodeInfo.Name, "drained")
	return nil
}

// getPodsForDeletion returns all the pods we're going to delete.  If there are
// any unmanaged pods and the user didn't pass --force, we return that list in
// an error.
func (o *DrainOptions) getPodsForDeletion() ([]api.Pod, error) {
	pods := []api.Pod{}
	podList, err := o.client.Pods(api.NamespaceAll).List(api.ListOptions{FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": o.nodeInfo.Name})})
	if err != nil {
		return pods, err
	}
	unreplicatedPodNames := []string{}

	for _, pod := range podList.Items {
		_, found := pod.ObjectMeta.Annotations[types.ConfigMirrorAnnotationKey]
		if found {
			// Skip mirror pod
			continue
		}
		replicated := false

		creatorRef, found := pod.ObjectMeta.Annotations[controller.CreatedByAnnotation]
		if found {
			// Now verify that the specified creator actually exists.
			var sr api.SerializedReference
			if err := runtime.DecodeInto(o.factory.Decoder(true), []byte(creatorRef), &sr); err != nil {
				return pods, err
			}
			if sr.Reference.Kind == "ReplicationController" {
				rc, err := o.client.ReplicationControllers(sr.Reference.Namespace).Get(sr.Reference.Name)
				// Assume the only reason for an error is because the RC is
				// gone/missing, not for any other cause.  TODO(mml): something more
				// sophisticated than this
				if err == nil && rc != nil {
					replicated = true
				}
			} else if sr.Reference.Kind == "DaemonSet" {
				ds, err := o.client.DaemonSets(sr.Reference.Namespace).Get(sr.Reference.Name)

				// Assume the only reason for an error is because the DaemonSet is
				// gone/missing, not for any other cause.  TODO(mml): something more
				// sophisticated than this
				if err == nil && ds != nil {
					replicated = true
				}
			} else if sr.Reference.Kind == "Job" {
				job, err := o.client.Jobs(sr.Reference.Namespace).Get(sr.Reference.Name)

				// Assume the only reason for an error is because the Job is
				// gone/missing, not for any other cause.  TODO(mml): something more
				// sophisticated than this
				if err == nil && job != nil {
					replicated = true
				}
			}
		}
		if replicated || o.Force {
			pods = append(pods, pod)
		}
		if !replicated {
			unreplicatedPodNames = append(unreplicatedPodNames, pod.Name)
		}
	}

	if len(unreplicatedPodNames) > 0 {
		joined := strings.Join(unreplicatedPodNames, ", ")
		if !o.Force {
			return pods, fmt.Errorf("refusing to continue due to pods managed by neither a ReplicationController, nor a Job, nor a DaemonSet: %s (use --force to override)", joined)
		}
		fmt.Fprintf(o.out, "WARNING: About to delete these pods managed by neither a ReplicationController, nor a Job, nor a DaemonSet: %s\n", joined)
	}
	return pods, nil
}

// deletePods deletes the pods on the api server
func (o *DrainOptions) deletePods(pods []api.Pod) error {
	deleteOptions := api.DeleteOptions{}
	if o.GracePeriodSeconds >= 0 {
		gracePeriodSeconds := int64(o.GracePeriodSeconds)
		deleteOptions.GracePeriodSeconds = &gracePeriodSeconds
	}

	for _, pod := range pods {
		err := o.client.Pods(pod.Namespace).Delete(pod.Name, &deleteOptions)
		if err != nil {
			return err
		}
		cmdutil.PrintSuccess(o.mapper, false, o.out, "pod", pod.Name, "deleted")
	}

	return nil
}

// RunCordonOrUncordon runs either Cordon or Uncordon.  The desired value for
// "Unschedulable" is passed as the first arg.
func (o *DrainOptions) RunCordonOrUncordon(desired bool) error {
	cmdNamespace, _, err := o.factory.DefaultNamespace()
	if err != nil {
		return err
	}

	if o.nodeInfo.Mapping.GroupVersionKind.Kind == "Node" {
		unsched := reflect.ValueOf(o.nodeInfo.Object).Elem().FieldByName("Spec").FieldByName("Unschedulable")
		if unsched.Bool() == desired {
			cmdutil.PrintSuccess(o.mapper, false, o.out, o.nodeInfo.Mapping.Resource, o.nodeInfo.Name, already(desired))
		} else {
			helper := resource.NewHelper(o.client, o.nodeInfo.Mapping)
			unsched.SetBool(desired)
			_, err := helper.Replace(cmdNamespace, o.nodeInfo.Name, true, o.nodeInfo.Object)
			if err != nil {
				return err
			}
			cmdutil.PrintSuccess(o.mapper, false, o.out, o.nodeInfo.Mapping.Resource, o.nodeInfo.Name, changed(desired))
		}
	} else {
		cmdutil.PrintSuccess(o.mapper, false, o.out, o.nodeInfo.Mapping.Resource, o.nodeInfo.Name, "skipped")
	}

	return nil
}

// already() and changed() return suitable strings for {un,}cordoning

func already(desired bool) string {
	if desired {
		return "already cordoned"
	}
	return "already uncordoned"
}

func changed(desired bool) string {
	if desired {
		return "cordoned"
	}
	return "uncordoned"
}
