package kube_state

import (
	"context"
	"time"

	"github.com/influxdata/telegraf"
	"k8s.io/api/core/v1"
)

var namespaceMeasurement = "kube_namespace"

func registerNamespaceCollector(ctx context.Context, acc telegraf.Accumulator, ks *KubenetesState) {
	list, err := ks.client.getNamespaces(ctx)
	if err != nil {
		acc.AddError(err)
		return
	}
	for _, ns := range list.Items {
		if err = ks.gatherNamespace(ns, acc); err != nil {
			acc.AddError(err)
			return
		}
	}
}

func (ks *KubenetesState) gatherNamespace(ns v1.Namespace, acc telegraf.Accumulator) error {
	var creationTime time.Time
	if !ns.CreationTimestamp.IsZero() {
		creationTime = ns.CreationTimestamp.Time
	}
	fields := map[string]interface{}{
		"gauge": 1,
	}
	tags := map[string]string{
		"namespace":    ns.Name,
		"status_phase": string(ns.Status.Phase),
	}
	for k, v := range ns.Labels {
		tags["label_"+sanitizeLabelName(k)] = v
	}
	for k, v := range ns.Annotations {
		tags["annotation_"+sanitizeLabelName(k)] = v
	}
	acc.AddFields(namespaceMeasurement, fields, tags, creationTime)

	return nil
}
