package kube_state

import (
	"context"
	"time"

	"github.com/influxdata/telegraf"
	autoscaling "k8s.io/api/autoscaling/v2beta1"
	"k8s.io/api/core/v1"
)

var horizontalPodAutoScalerMeasurement = "kube_hpa"
var horizontalPodAutoScalerStatusMeasurement = "kube_hpa_status"

func registerHorizontalPodAutoScalerCollector(ctx context.Context, acc telegraf.Accumulator, ks *KubenetesState) {
	list, err := ks.client.gethorizontalPodAutoScalers(ctx)
	if err != nil {
		acc.AddError(err)
		return
	}
	for _, h := range list.Items {
		if err = ks.gatherHorizontalPodAutoscaler(h, acc); err != nil {
			acc.AddError(err)
			return
		}
	}
}

func (ks *KubenetesState) gatherHorizontalPodAutoscaler(h autoscaling.HorizontalPodAutoscaler, acc telegraf.Accumulator) error {
	var creationTime time.Time
	if !h.CreationTimestamp.IsZero() {
		creationTime = h.CreationTimestamp.Time
	}
	fields := map[string]interface{}{
		"metadata_generation": h.ObjectMeta.Generation,
		"spec_max_replicas":   h.Spec.MaxReplicas,
		"spec_min_replicas":   h.Spec.MinReplicas,
	}
	tags := map[string]string{
		"namespace": h.Namespace,
		"hpa":       h.Name,
	}
	for k, v := range h.Labels {
		tags["label_"+sanitizeLabelName(k)] = v
	}
	for _, c := range h.Status.Conditions {
		ks.gatherHorizontalPodAutoScalerStatusCondition(c, h, acc)
	}
	acc.AddFields(horizontalPodAutoScalerMeasurement, fields, tags, creationTime)

	return nil
}

func (ks *KubenetesState) gatherHorizontalPodAutoScalerStatusCondition(
	c autoscaling.HorizontalPodAutoscalerCondition,
	h autoscaling.HorizontalPodAutoscaler,
	acc telegraf.Accumulator) {
	fields := map[string]interface{}{
		"current_replicas": h.Status.CurrentReplicas,
		"desired_replicas": h.Status.DesiredReplicas,
		"condition_true":   boolInt(c.Status == v1.ConditionTrue),
		"condition_false":  boolInt(c.Status == v1.ConditionFalse),
		"condition_unkown": boolInt(c.Status == v1.ConditionUnknown),
	}
	tags := map[string]string{
		"namespace": h.Namespace,
		"hpa":       h.Name,
		"condition": string(c.Type),
	}
	acc.AddFields(horizontalPodAutoScalerStatusMeasurement, fields, tags)

}
