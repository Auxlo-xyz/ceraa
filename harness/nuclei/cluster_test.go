package nuclei

import (
	"context"
	"strings"
	"testing"

	"ceraa/harness/recon"
)

// clusterProbe answers with a body per payload.
//
// The baseline answers with the blocked body and every payload answers
// with the allowed one, which is the shape a cluster attack looks for.
func clusterProbe(t *testing.T, blocked string, allowed string) ProbeFunc {
	t.Helper()
	return func(_ context.Context, req recon.Request) (recon.Result, []byte, error) {
		body := allowed
		if strings.Contains(req.URL, "payload") || !strings.Contains(req.URL, "?") {
			body = blocked
		}
		return recon.Result{
			URL:       req.URL,
			Method:    req.Method,
			Status:    200,
			Headers:   map[string]string{"content-type": "text/html"},
			BodyBytes: len(body),
			SHA256:    "clusterhash",
			ReceiptID: "receipt-cluster.json",
		}, []byte(body), nil
	}
}

// TestTheClusterFindsTheOutlier: the payload whose answer differs from the
// baseline is the finding.
func TestTheClusterFindsTheOutlier(t *testing.T) {
	cluster := Cluster{
		Method:    "GET",
		Payloads:  map[string]string{"admin": "admin"},
		Attribute: "word",
		Words:     []string{"blocked"},
	}
	baseline := ClusterResponse{Status: 200, Body: []byte("blocked by the filter"), URL: "https://example.com/admin"}
	payloads := []ClusterPayloadResponse{{
		Name:     "admin",
		Value:    "admin",
		Response: ClusterResponse{Status: 200, Body: []byte("welcome admin"), URL: "https://example.com/admin?payload=admin"},
	}}
	result := CompareCluster(cluster, baseline, payloads)
	if !result.Matched {
		t.Fatalf("the cluster did not fire: %+v", result)
	}
	if len(result.Outliers) != 1 {
		t.Fatalf("the cluster found %d outliers, want 1", len(result.Outliers))
	}
	outlier := result.Outliers[0]
	if outlier.Payload != "admin" {
		t.Errorf("the outlier is %q, want the payload name", outlier.Payload)
	}
	if !strings.Contains(outlier.Difference, "blocked") {
		t.Errorf("the difference does not carry the values: %q", outlier.Difference)
	}
	if !strings.Contains(outlier.Difference, "admin") {
		t.Errorf("the difference does not name the payload: %q", outlier.Difference)
	}
}

// TestTheClusterBaselineHolds: a payload that answers like the baseline is
// not a finding.
func TestTheClusterBaselineHolds(t *testing.T) {
	cluster := Cluster{
		Payloads:  map[string]string{"admin": "admin"},
		Attribute: "word",
		Words:     []string{"blocked"},
	}
	baseline := ClusterResponse{Status: 200, Body: []byte("blocked by the filter")}
	payloads := []ClusterPayloadResponse{{
		Name:     "admin",
		Response: ClusterResponse{Status: 200, Body: []byte("blocked by the filter")},
	}}
	result := CompareCluster(cluster, baseline, payloads)
	if result.Matched {
		t.Errorf("the cluster fired on a payload that answered like the baseline: %+v", result)
	}
	if len(result.Outliers) != 0 {
		t.Errorf("the cluster found %d outliers, want none", len(result.Outliers))
	}
}

// TestTheClusterHonoursTheCondition: "or" needs one payload to differ and
// "and" needs every one.
func TestTheClusterHonoursTheCondition(t *testing.T) {
	cluster := Cluster{
		Payloads:  map[string]string{"one": "one", "two": "two"},
		Attribute: "word",
		Words:     []string{"blocked"},
	}
	baseline := ClusterResponse{Status: 200, Body: []byte("blocked by the filter")}
	payloads := []ClusterPayloadResponse{
		{Name: "one", Response: ClusterResponse{Status: 200, Body: []byte("welcome")}},
		{Name: "two", Response: ClusterResponse{Status: 200, Body: []byte("blocked by the filter")}},
	}
	cluster.Condition = "or"
	result := CompareCluster(cluster, baseline, payloads)
	if !result.Matched {
		t.Errorf("the or condition did not fire on the one payload that differed: %+v", result)
	}
	cluster.Condition = "and"
	result = CompareCluster(cluster, baseline, payloads)
	if result.Matched {
		t.Errorf("the and condition fired with one payload still like the baseline: %+v", result)
	}
}

// TestTheClusterHonoursTheAttribute: the status and the size are compared
// when the template asks for them.
func TestTheClusterHonoursTheAttribute(t *testing.T) {
	baseline := ClusterResponse{Status: 200, Body: []byte("the same body")}
	payload := []ClusterPayloadResponse{{
		Name:     "one",
		Response: ClusterResponse{Status: 500, Body: []byte("a much longer body")},
	}}
	status := Cluster{Attribute: "status", Payloads: map[string]string{"one": "one"}}
	result := CompareCluster(status, baseline, payload)
	if !result.Matched {
		t.Errorf("the status attribute did not find the difference: %+v", result)
	}
	if !strings.Contains(result.Outliers[0].Difference, "500") {
		t.Errorf("the difference does not carry the status: %q", result.Outliers[0].Difference)
	}
	size := Cluster{Attribute: "size", Payloads: map[string]string{"one": "one"}}
	result = CompareCluster(size, baseline, payload)
	if !result.Matched {
		t.Errorf("the size attribute did not find the difference: %+v", result)
	}
	if !strings.Contains(result.Outliers[0].Difference, "size") {
		t.Errorf("the difference does not name the attribute: %q", result.Outliers[0].Difference)
	}
}

// TestTheClusterFiresTheBaselineAndOneRequestPerPayload: the baseline is
// the request with no payload.
func TestTheClusterFiresTheBaselineAndOneRequestPerPayload(t *testing.T) {
	cluster := Cluster{
		Method:    "GET",
		Payloads:  map[string]string{"admin": "admin", "root": "root"},
		Attribute: "word",
		Words:     []string{"blocked"},
	}
	fires, err := ClusterRequests(cluster, ClusterRequest{
		Method: "GET",
		Path:   "https://example.com/login",
	}, nil)
	if err != nil {
		t.Fatalf("ClusterRequests: %v", err)
	}
	if len(fires) != 3 {
		t.Fatalf("the cluster built %d requests, want the baseline and one per payload", len(fires))
	}
	if fires[0].Payload != "" {
		t.Errorf("the first request is %q, want the baseline", fires[0].Payload)
	}
	if fires[0].Request.URL != "https://example.com/login" {
		t.Errorf("the baseline went to %q", fires[0].Request.URL)
	}
	names := map[string]bool{}
	for _, fire := range fires[1:] {
		names[fire.Payload] = true
		if !strings.Contains(fire.Request.URL, fire.Value) {
			t.Errorf("the payload %q did not reach the request: %q", fire.Payload, fire.Request.URL)
		}
	}
	if !names["admin"] || !names["root"] {
		t.Errorf("the payloads were not built: %v", names)
	}
}

// TestTheClusterPayloadReachesThePlaceholder: a request that names
// {{payload}} gets the value there.
func TestTheClusterPayloadReachesThePlaceholder(t *testing.T) {
	cluster := Cluster{Payloads: map[string]string{"admin": "admin"}}
	fires, err := ClusterRequests(cluster, ClusterRequest{
		Method: "POST",
		Path:   "https://example.com/login",
		Body:   "user={{payload}}",
	}, nil)
	if err != nil {
		t.Fatalf("ClusterRequests: %v", err)
	}
	if len(fires) != 2 {
		t.Fatalf("the cluster built %d requests, want the baseline and one payload", len(fires))
	}
	if fires[1].Request.Body != "user=admin" {
		t.Errorf("the payload body is %q", fires[1].Request.Body)
	}
	if strings.Contains(fires[1].Request.URL, "payload=") {
		t.Errorf("the payload was also appended to the url: %q", fires[1].Request.URL)
	}
}

// TestTheClusterRunsAgainstAResponse: the whole attack, from the requests
// to the comparison.
func TestTheClusterRunsAgainstAResponse(t *testing.T) {
	cluster := Cluster{
		Method:    "GET",
		Payloads:  map[string]string{"admin": "admin"},
		Attribute: "word",
		Words:     []string{"blocked"},
	}
	probe := func(_ context.Context, req recon.Request) (recon.Result, []byte, error) {
		body := "welcome admin"
		if !strings.Contains(req.URL, "?") {
			body = "blocked by the filter"
		}
		return recon.Result{URL: req.URL, Status: 200, SHA256: "hash"}, []byte(body), nil
	}
	result, err := RunCluster(context.Background(), cluster, ClusterRequest{
		Method: "GET",
		Path:   "https://example.com/login",
	}, nil, probe)
	if err != nil {
		t.Fatalf("RunCluster: %v", err)
	}
	if !result.Matched {
		t.Fatalf("the cluster did not fire: %+v", result)
	}
	if len(result.Outliers) != 1 || result.Outliers[0].Payload != "admin" {
		t.Errorf("the outliers are %+v, want the payload that got past the filter", result.Outliers)
	}
}
