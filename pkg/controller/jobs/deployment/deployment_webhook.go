/*
Copyright 2024 The Kubernetes Authors.

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

package deployment

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"sigs.k8s.io/kueue/pkg/controller/constants"
	"sigs.k8s.io/kueue/pkg/controller/jobframework"
	"sigs.k8s.io/kueue/pkg/controller/jobframework/webhook"
)

type Webhook struct {
	client                     client.Client
	manageJobsWithoutQueueName bool
}

func SetupWebhook(mgr ctrl.Manager, opts ...jobframework.Option) error {
	options := jobframework.ProcessOptions(opts...)
	wh := &Webhook{
		client:                     mgr.GetClient(),
		manageJobsWithoutQueueName: options.ManageJobsWithoutQueueName,
	}
	obj := &appsv1.Deployment{}
	return webhook.WebhookManagedBy(mgr).
		For(obj).
		WithMutationHandler(webhook.WithLosslessDefaulter(mgr.GetScheme(), obj, wh)).
		WithValidator(wh).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-apps-v1-deployment,mutating=true,failurePolicy=fail,sideEffects=None,groups="apps",resources=deployments,verbs=create,versions=v1,name=mdeployment.kb.io,admissionReviewVersions=v1

var _ admission.CustomDefaulter = &Webhook{}

func (wh *Webhook) Default(ctx context.Context, obj runtime.Object) error {
	d := fromObject(obj)
	log := ctrl.LoggerFrom(ctx).WithName("deployment-webhook").WithValues("deployment", klog.KObj(d))
	log.V(5).Info("Applying defaults")

	cqLabel, ok := d.Labels[constants.QueueLabel]
	if ok {
		if d.Spec.Template.Labels == nil {
			d.Spec.Template.Labels = make(map[string]string, 1)
		}
		d.Spec.Template.Labels[constants.QueueLabel] = cqLabel
	}

	return nil
}

// +kubebuilder:webhook:path=/validate-apps-v1-deployment,mutating=false,failurePolicy=fail,sideEffects=None,groups="apps",resources=deployments,verbs=create;update,versions=v1,name=vdeployment.kb.io,admissionReviewVersions=v1

var _ admission.CustomValidator = &Webhook{}

func (wh *Webhook) ValidateCreate(context.Context, runtime.Object) (warnings admission.Warnings, err error) {
	return nil, nil
}

var (
	deploymentLabelsPath         = field.NewPath("metadata", "labels")
	deploymentQueueNameLabelPath = deploymentLabelsPath.Key(constants.QueueLabel)

	podSpecQueueNameLabelPath = field.NewPath("spec", "template", "metadata", "labels").
					Key(constants.QueueLabel)
)

func (wh *Webhook) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (warnings admission.Warnings, err error) {
	oldDeployment := fromObject(oldObj)
	newDeployment := fromObject(newObj)

	log := ctrl.LoggerFrom(ctx).WithName("deployment-webhook").WithValues("deployment", klog.KObj(newDeployment))
	log.V(5).Info("Validating update")
	allErrs := apivalidation.ValidateImmutableField(
		newDeployment.GetLabels()[constants.QueueLabel],
		oldDeployment.GetLabels()[constants.QueueLabel],
		deploymentQueueNameLabelPath,
	)
	allErrs = append(allErrs, apivalidation.ValidateImmutableField(
		newDeployment.Spec.Template.GetLabels()[constants.QueueLabel],
		oldDeployment.Spec.Template.GetLabels()[constants.QueueLabel],
		podSpecQueueNameLabelPath,
	)...)

	return warnings, allErrs.ToAggregate()
}

func (wh *Webhook) ValidateDelete(context.Context, runtime.Object) (warnings admission.Warnings, err error) {
	return nil, nil
}
