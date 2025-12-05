package operator

import (
	"encoding/json"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	zv1 "github.com/zalando-incubator/es-operator/pkg/apis/zalando.org/v1"
	"github.com/zalando-incubator/es-operator/pkg/client/clientset/versioned/fake"
	"github.com/zalando-incubator/es-operator/pkg/clientset"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8s_fake "k8s.io/client-go/kubernetes/fake"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestAutoScalerAndOperatorIntegration(t *testing.T) {
	log.SetLevel(log.DebugLevel)

	// Mock ES Server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/_cluster/health":
			fmt.Fprintln(w, `{"status": "green"}`)
		case "/_cat/indices":
			fmt.Fprintln(w, `[{"index": "idx1", "pri": "6", "rep": "2"}]`)
		case "/_cat/shards":
			shards := []map[string]string{}
			for i := range 42 {
				shards = append(shards, map[string]string{
					"index": "idx1",
					"ip":    fmt.Sprintf("1.2.3.%d", i%6),
				})
			}
			json.NewEncoder(w).Encode(shards)
		case "/_cat/nodes":
			nodes := []map[string]any{}
			for i := range 10 {
				nodes = append(nodes, map[string]any{
					"ip":  fmt.Sprintf("1.2.3.%d", i),
					"dup": "20.0",
				})
			}
			json.NewEncoder(w).Encode(nodes)
		case "/_cluster/settings":
			fmt.Fprintln(w, `{"persistent": {}, "transient": {}}`)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer ts.Close()
	endpoint, _ := url.Parse(ts.URL)

	// Setup Resources
	replicas6 := int32(6)
	replicas10 := int32(10)

	eds := &zv1.ElasticsearchDataSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-eds",
			Namespace: "default",
			UID:       types.UID("eds-uid"),
			Annotations: map[string]string{
				esOperatorAnnotationKey: "", // Owned by default operator
			},
		},
		Spec: zv1.ElasticsearchDataSetSpec{
			Replicas: &replicas6,
			Scaling: &zv1.ElasticsearchDataSetScaling{
				Enabled:                           true,
				MinReplicas:                       6,
				MaxReplicas:                       20,
				MinIndexReplicas:                  2,
				MaxIndexReplicas:                  6,
				MinShardsPerNode:                  1,
				MaxShardsPerNode:                  3,
				ScaleUpThresholdDurationSeconds:   120,
				ScaleDownThresholdDurationSeconds: 600,
				ScaleUpCooldownSeconds:            300,
				ScaleUpCPUBoundary:                50,
				ScaleDownCPUBoundary:              20,
				ScaleDownCooldownSeconds:          900,
			},
		},
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-eds",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					UID:        "eds-uid",
					Kind:       "ElasticsearchDataSet",
					APIVersion: "zalando.org/v1",
					Name:       "test-eds",
				},
			},
			Labels: map[string]string{
				"es-operator-dataset": "test-eds",
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas10,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"es-operator-dataset": "test-eds"},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"es-operator-dataset": "test-eds"},
				},
			},
		},
		Status: appsv1.StatefulSetStatus{
			ReadyReplicas:  10,
			UpdateRevision: "hash",
		},
	}

	// Pods
	pods := []runtime.Object{}
	for i := range 10 {
		pods = append(pods, &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:         fmt.Sprintf("test-eds-%d", i),
				GenerateName: "test-eds-",
				Namespace:    "default",
				Labels: map[string]string{
					"es-operator-dataset":      "test-eds",
					"controller-revision-hash": "hash",
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						Kind: "StatefulSet",
						Name: "test-eds",
						UID:  sts.UID,
					},
				},
			},
			Status: v1.PodStatus{
				PodIP: fmt.Sprintf("1.2.3.%d", i),
				Phase: v1.PodRunning,
			},
			Spec: v1.PodSpec{
				NodeName:                      "node-1",
				TerminationGracePeriodSeconds: new(int64),
			},
		})
	}

	// Nodes
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
			Labels: map[string]string{
				"foo": "bar",
			},
		},
	}

	// Metrics triggering scale up
	metricSet := &zv1.ElasticsearchMetricSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-eds",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					UID: "eds-uid",
				},
			},
		},
		Metrics: []zv1.ElasticsearchMetric{
			{
				Timestamp: metav1.Now(),
				Value:     20,
			},
			{
				Timestamp: metav1.Now(),
				Value:     30,
			},
			{
				Timestamp: metav1.Now(),
				Value:     33,
			},
			{
				Timestamp: metav1.Now(),
				Value:     35,
			},
			{
				Timestamp: metav1.Now(),
				Value:     45,
			},
			{
				Timestamp: metav1.Now(),
				Value:     55,
			},
			{
				Timestamp: metav1.Now(),
				Value:     60,
			},
			{
				Timestamp: metav1.Now(),
				Value:     70,
			},
			{
				Timestamp: metav1.Now(),
				Value:     80,
			},
			{
				Timestamp: metav1.Now(),
				Value:     90,
			},
		},
	}

	objs := append(pods, sts, node)
	k8sClient := k8s_fake.NewSimpleClientset(objs...)
	zClient := fake.NewSimpleClientset(eds, metricSet)
	client := clientset.NewCustomClientset(k8sClient, zClient, nil)

	op := NewElasticsearchOperator(
		client,
		nil,
		time.Hour,
		1*time.Second,
		"",
		"",
		"cluster.local",
		endpoint,
	)

	// Setup Informers
	ctx := t.Context()
	err := op.setupInformers(ctx)
	require.NoError(t, err)

	// Collect resources manually
	resources, err := op.collectResources(ctx)
	require.NoError(t, err)
	esRes := resources[eds.UID]
	require.NotNil(t, esRes)
	require.NotNil(t, esRes.MetricSet)

	esClient := &ESClient{Endpoint: endpoint}

	require.Nil(t, eds.Status.LastScaleUpStarted)

	err = op.scaleEDS(ctx, eds, esRes, esClient)
	require.NoError(t, err)

	// Verify EDS is updated
	updatedEDS, err := zClient.ZalandoV1().ElasticsearchDataSets("default").Get(ctx, "test-eds", metav1.GetOptions{})
	require.NoError(t, err)

	// Metrics (80) > Threshold (50) -> Scale UP.
	// 6 -> 9.
	assert.Equal(t, int32(9), *updatedEDS.Spec.Replicas)

	// operateEDS -> go operator.Run(...)
	err = op.operateEDS(updatedEDS, false)
	require.NoError(t, err)

	// Wait for STS to be updated
	require.Eventually(t, func() bool {
		updatedSTS, err := k8sClient.AppsV1().StatefulSets("default").Get(ctx, "test-eds", metav1.GetOptions{})
		if err != nil {
			return false
		}
		return *updatedSTS.Spec.Replicas == 9
	}, 10*time.Second, 100*time.Millisecond)

	updatedEDS, err = zClient.ZalandoV1().ElasticsearchDataSets("default").Get(ctx, "test-eds", metav1.GetOptions{})
	require.Nil(t, err)
	require.Nil(t, updatedEDS.Status.LastScaleUpStarted)
	require.NotNil(t, updatedEDS.Status.LastScaleDownStarted)
	require.Nil(t, updatedEDS.Status.LastScaleDownEnded)

	// Second round, still scaling up (metricset unchanged)
	resources, err = op.collectResources(ctx)
	require.NoError(t, err)
	esRes = resources[eds.UID]
	require.NotNil(t, esRes)

	err = op.scaleEDS(ctx, updatedEDS, esRes, esClient)
	require.NoError(t, err)

	updatedEDS, err = zClient.ZalandoV1().ElasticsearchDataSets("default").Get(ctx, "test-eds", metav1.GetOptions{})
	require.NoError(t, err)
	// Logic: 9 -> 18.
	assert.Equal(t, int32(18), *updatedEDS.Spec.Replicas)
	require.NotNil(t, updatedEDS.Status.LastScaleUpStarted)
}
