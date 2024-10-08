// Copyright KubeArchive Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	ceOtelObs "github.com/cloudevents/sdk-go/observability/opentelemetry/v2/client"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	ceClient "github.com/cloudevents/sdk-go/v2/client"
	"github.com/kubearchive/kubearchive/cmd/sink/filters"
	"github.com/kubearchive/kubearchive/cmd/sink/k8s"
	"github.com/kubearchive/kubearchive/pkg/database"
	"github.com/kubearchive/kubearchive/pkg/files"
	"github.com/kubearchive/kubearchive/pkg/models"
	kaObservability "github.com/kubearchive/kubearchive/pkg/observability"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
)

var (
	version = "main"
	commit  = ""
	date    = ""
)

const (
	otelServiceName = "kubearchive.sink"
	mountPathEnvVar = "MOUNT_PATH"
)

type Sink struct {
	Db          database.DBInterface
	EventClient ceClient.Client
	Filters     *filters.Filters
	K8sClient   *dynamic.DynamicClient
}

func NewSink(db database.DBInterface, filters *filters.Filters) *Sink {
	if db == nil {
		slog.Error("Cannot start sink when db connection is nil")
		os.Exit(1)
	}

	httpClient, err := cloudevents.NewHTTP(
		cloudevents.WithRoundTripper(otelhttp.NewTransport(http.DefaultTransport)),
		cloudevents.WithMiddleware(func(next http.Handler) http.Handler {
			return otelhttp.NewHandler(next, "receive")
		}),
	)
	if err != nil {
		slog.Error("Failed to create HTTP client", "err", err.Error())
		os.Exit(1)
	}
	eventClient, err := cloudevents.NewClient(httpClient, ceClient.WithObservabilityService(ceOtelObs.NewOTelObservabilityService()))
	if err != nil {
		slog.Error("Failed to create CloudEvents HTTP client", "err", err.Error())
		os.Exit(1)
	}

	k8sClient, err := k8s.GetKubernetesClient()
	if err != nil {
		slog.Error("Could not start a kubernetes client", "err", err)
		os.Exit(1)
	}

	return &Sink{
		Db:          db,
		EventClient: eventClient,
		Filters:     filters,
		K8sClient:   k8sClient,
	}
}

// Processes incoming cloudevents and writes them to the database
func (sink *Sink) Receive(ctx context.Context, event cloudevents.Event) {
	slog.Info("Received cloudevent", "id", event.ID())
	k8sObj, err := models.UnstructuredFromByteSlice(event.Data())
	if err != nil {
		slog.Error("Cloudevent is malformed and will not be processed", "id", event.ID(), "err", err)
		return
	}
	slog.Info(
		"Cloudevent contains all required fields. Checking if its object needs to be archived",
		"id", event.ID(),
		"objectID", k8sObj.GetUID(),
	)

	if strings.HasSuffix(event.Type(), ".delete") {
		slog.Info(
			"The type of cloudevent is Delete. Checking if object needs to be archived",
			"id", event.ID(),
			"objectID", k8sObj.GetUID(),
		)
		if sink.Filters.MustArchiveOnDelete(ctx, k8sObj) {
			ctx, cancel := context.WithTimeout(ctx, time.Second*5)
			err = sink.Db.WriteResource(ctx, k8sObj, event.Data())
			if err != nil {
				slog.Error(
					"Failed to write object from cloudevent to the database",
					"objectID", k8sObj.GetUID(),
					"id", event.ID(),
					"err", err,
				)
			}
			defer cancel()
			return
		}
		slog.Info(
			"Object from cloudevent does not need to be archived after deletion",
			"objectID", k8sObj.GetUID(),
			"id", event.ID(),
		)
		return
	}
	if !sink.Filters.MustArchive(ctx, k8sObj) {
		slog.Info(
			"Object from cloudevent does not need to be archived",
			"objectID", k8sObj.GetUID(),
			"id", event.ID(),
		)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second*5)
	slog.Info(
		"Writing object from cloudevent into the database",
		"objectID", k8sObj.GetUID(),
		"id", event.ID(),
	)
	err = sink.Db.WriteResource(ctx, k8sObj, event.Data())
	defer cancel()
	if err != nil {
		slog.Error(
			"Failed to write object from cloudevent to the database",
			"objectID", k8sObj.GetUID(),
			"id", event.ID(),
			"err", err,
		)
		return
	}
	slog.Info(
		"Successfully wrote object from cloudevent to the database",
		"objectID", k8sObj.GetUID(),
		"id", event.ID(),
	)
	slog.Info(
		"Checking if object from cloudevent needs to be deleted",
		"objectID", k8sObj.GetUID(),
		"id", event.ID(),
	)
	if sink.Filters.MustDelete(ctx, k8sObj) {
		slog.Info("Attempting to delete kubernetes object", "objectID", k8sObj.GetUID())
		kind := k8sObj.GetObjectKind().GroupVersionKind()
		resource, _ := meta.UnsafeGuessKindToResource(kind)     // we only need the plural resource
		propagationPolicy := metav1.DeletePropagationBackground // can't get address of a const
		k8sCtx, k8sCancel := context.WithTimeout(ctx, time.Second*5)
		defer k8sCancel()
		err = sink.K8sClient.Resource(resource).Namespace(k8sObj.GetNamespace()).Delete(
			k8sCtx,
			k8sObj.GetName(),
			metav1.DeleteOptions{PropagationPolicy: &propagationPolicy},
		)
		if err != nil {
			slog.Error(
				"Could not delete object",
				"objectID", k8sObj.GetUID(),
				"err", err,
			)
			return
		}
		slog.Info("Successfully requested kubernetes object be deleted", "objectID", k8sObj.GetUID())
		deleteTs := metav1.Now()
		k8sObj.SetDeletionTimestamp(&deleteTs)
		slog.Info("Updating cluster_deleted_ts for kubernetes object", "objectID", k8sObj.GetUID())
		updateCtx, updateCancel := context.WithTimeout(ctx, time.Second*5)
		err = sink.Db.WriteResource(updateCtx, k8sObj, event.Data())
		defer updateCancel()
		if err != nil {
			slog.Error("Failed to update cluster_deleted_ts for kubernetes object", "objectID", k8sObj.GetUID())
			return
		}
		slog.Info("Successfully deleted kubernetes object", "objectID", k8sObj.GetUID())
	} else {
		slog.Info(
			"Object from cloudevent does not need to be deleted",
			"objectID", k8sObj.GetUID(),
			"id", event.ID(),
		)
	}
}

func main() {
	slog.Info("Starting KubeArchive Sink", "version", version, "commit", commit, "built", date)

	err := kaObservability.Start(otelServiceName)
	if err != nil {
		slog.Error("Could not start tracing", "err", err.Error())
		os.Exit(1)
	}
	db, err := database.NewDatabase()
	if err != nil {
		slog.Error("Could not connect to the database", "err", err)
		os.Exit(1)
	}
	defer func(db database.DBInterface) {
		err := db.CloseDB()
		if err != nil {
			slog.Error("Could not close the database connection", "error", err.Error())
		} else {
			slog.Info("Connection closed successfully")
		}
	}(db)

	filters, err := filters.NewFilters()
	if err != nil {
		slog.Error(
			"Not all filters could be created from the ConfigMap. Some archive and delete operations will not execute until the errors are resolved",
			"err", err,
		)
	}
	stopUpdating, err := files.UpdateOnPaths(filters.Update, filters.Path())
	if err != nil {
		slog.Error("Could not listen for updates to filters", "err", err)
	}
	defer func() {
		err := stopUpdating()
		if err != nil {
			slog.Error("Encountered an issue stopping filter updates", "err", err)
		}
	}()
	sink := NewSink(db, filters)
	err = sink.EventClient.StartReceiver(context.Background(), sink.Receive)
	if err != nil {
		slog.Error("Failed to start receiving CloudEvents", "err", err.Error())
		os.Exit(1)
	}
}
