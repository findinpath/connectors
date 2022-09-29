package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bradleyjkemp/cupaloy"
	pc "github.com/estuary/flow/go/protocols/capture"
	"github.com/estuary/flow/go/protocols/flow"
	jsonpatch "github.com/evanphx/json-patch/v5"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

type testCaptureSpec struct {
	Driver       pc.DriverServer
	EndpointSpec interface{}
	Bindings     []*flow.CaptureSpec_Binding
	Checkpoint   json.RawMessage
}

func (cs *testCaptureSpec) Discover(ctx context.Context, t testing.TB, matchers ...*regexp.Regexp) {
	t.Helper()
	if os.Getenv("RUN_CAPTURES") != "yes" {
		t.Skipf("skipping %q capture: ${RUN_CAPTURES} != \"yes\"", t.Name())
	}

	endpointSpecJSON, err := json.Marshal(cs.EndpointSpec)
	require.NoError(t, err)

	discovery, err := cs.Driver.Discover(ctx, &pc.DiscoverRequest{
		EndpointSpecJson: endpointSpecJSON,
	})
	require.NoError(t, err)

	var matchedBindings = make(map[string]*pc.DiscoverResponse_Binding)
	var matchedNames []string
	for _, binding := range discovery.Bindings {
		if matchers == nil || matchesAny(matchers, string(binding.ResourceSpecJson)) {
			var name = binding.RecommendedName.String()
			matchedNames = append(matchedNames, name)
			matchedBindings[name] = binding
		}
	}
	sort.Strings(matchedNames)

	var summary = new(strings.Builder)
	for idx, name := range matchedNames {
		fmt.Fprintf(summary, "Binding %d:\n", idx)
		bs, err := json.MarshalIndent(matchedBindings[name], "  ", "  ")
		require.NoError(t, err)
		io.Copy(summary, bytes.NewReader(bs))
		fmt.Fprintf(summary, "\n")

	}
	cupaloy.SnapshotT(t, summary.String())
}

func matchesAny(matchers []*regexp.Regexp, text string) bool {
	for _, matcher := range matchers {
		if matcher.MatchString(text) {
			return true
		}
	}
	return false
}

func (cs *testCaptureSpec) Verify(ctx context.Context, t testing.TB, sentinel string) {
	t.Helper()
	if os.Getenv("RUN_CAPTURES") != "yes" {
		t.Skipf("skipping %q capture: ${RUN_CAPTURES} != \"yes\"", t.Name())
	}
	log.WithFields(log.Fields{
		"checkpoint": cs.Checkpoint,
	}).Debug("running test capture")

	endpointSpecJSON, err := json.Marshal(cs.EndpointSpec)
	require.NoError(t, err)

	var open = &pc.PullRequest{
		Open: &pc.PullRequest_Open{
			DriverCheckpointJson: cs.Checkpoint,
			Capture: &flow.CaptureSpec{
				Capture:          flow.Capture("acmeCo/" + strings.Replace(t.Name(), "/", "-", -1) + "capture"),
				EndpointSpecJson: endpointSpecJSON,
				Bindings:         cs.Bindings,
			},
			Tail: true,
		},
	}

	ctx, shutdown := context.WithCancel(ctx)
	var adapter = &testPullAdapter{
		ctx:           ctx,
		openReq:       open,
		checkpoint:    cs.Checkpoint,
		bindings:      cs.Bindings,
		sentinelValue: sentinel,
		shutdown:      shutdown,
	}
	err = cs.Driver.Pull(adapter)
	cupaloy.SnapshotT(t, adapter.Summary(err))
	cs.Checkpoint = adapter.checkpoint
}

var SnapshotSanitizers = map[string]*regexp.Regexp{
	`"<TIMESTAMP>"`: regexp.MustCompile(`"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]*Z"`),
}

type testPullAdapter struct {
	ctx        context.Context
	openReq    *pc.PullRequest
	checkpoint []byte
	bindings   []*flow.CaptureSpec_Binding
	documents  map[uint32][]json.RawMessage // Map from binding index to list of documents

	// After the sentinel value is observed in an output document, the next state
	// checkpoint will cause the shutdown thunk to be invoked.
	sentinelReached bool
	sentinelValue   string
	shutdown        func()
}

func (a *testPullAdapter) Summary(err error) string {
	var out = new(strings.Builder)

	// Accumulate the contents of each buffer, in order of binding index.
	var indices []int
	for bindingIndex := range a.documents {
		indices = append(indices, int(bindingIndex))
	}
	sort.Ints(indices)
	for _, bindingIndex := range indices {
		var bindingName = "INVALID"
		if 0 <= bindingIndex && bindingIndex < len(a.bindings) {
			bindingName = strings.Join(a.bindings[bindingIndex].ResourcePath, "|")
		}
		fmt.Fprintf(out, "# ================================\n")
		fmt.Fprintf(out, "# Binding %d (%q)\n", bindingIndex, bindingName)
		fmt.Fprintf(out, "# ================================\n")
		var docs = a.documents[uint32(bindingIndex)]
		// Sort documents in lexicographic order
		sort.Slice(docs, func(i, j int) bool {
			return bytes.Compare(docs[i], docs[j]) < 0
		})
		for _, doc := range docs {
			out.WriteString(string(doc))
			out.WriteByte('\n')
		}
	}

	// Append the final state checkpoint
	fmt.Fprintf(out, "# ================================\n")
	fmt.Fprintf(out, "# Final State Checkpoint\n")
	fmt.Fprintf(out, "# ================================\n")
	out.WriteString(string(a.checkpoint))
	out.WriteByte('\n')

	// If the error result is non-nil, add that to the summary as well
	if err != nil {
		fmt.Fprintf(out, "# ================================\n")
		fmt.Fprintf(out, "# Capture Terminated With Error\n")
		fmt.Fprintf(out, "# ================================\n")
		out.WriteString(err.Error())
	}

	// Sanitize things like timestamps that might vary between test runs
	var summary = out.String()
	for sanitizeReplacement, sanitizeMatcher := range SnapshotSanitizers {
		summary = sanitizeMatcher.ReplaceAllString(summary, sanitizeReplacement)
	}
	return summary
}

func (a *testPullAdapter) Recv() (*pc.PullRequest, error) {
	if msg := a.openReq; msg != nil {
		a.openReq = nil
		return msg, nil
	}
	time.Sleep(10 * time.Millisecond)
	return &pc.PullRequest{Acknowledge: &pc.Acknowledge{}}, nil
}

func (a *testPullAdapter) Send(m *pc.PullResponse) error {
	if m.Checkpoint != nil {
		if !m.Checkpoint.Rfc7396MergePatch || a.checkpoint == nil {
			a.checkpoint = m.Checkpoint.DriverCheckpointJson
		} else if checkpoint, err := jsonpatch.MergePatch(a.checkpoint, m.Checkpoint.DriverCheckpointJson); err == nil {
			a.checkpoint = checkpoint
		} else {
			return fmt.Errorf("test error merging checkpoint: %w", err)
		}
		if logLevel := log.TraceLevel; log.IsLevelEnabled(logLevel) {
			if !bytes.Equal(m.Checkpoint.DriverCheckpointJson, []byte("{}")) {
				log.WithFields(log.Fields{
					"checkpoint": m.Checkpoint.DriverCheckpointJson,
					"patch":      m.Checkpoint.Rfc7396MergePatch,
					"result":     json.RawMessage(a.checkpoint),
				}).Log(logLevel, "checkpoint")
			}
		}
		if a.sentinelReached && !bytes.Equal(m.Checkpoint.DriverCheckpointJson, []byte("{}")) {
			a.shutdown()
		}
		return nil
	} else if m.Documents != nil {
		var binding = m.Documents.Binding
		if a.documents == nil {
			a.documents = make(map[uint32][]json.RawMessage)
		}
		for _, s := range m.Documents.DocsJson {
			var doc = json.RawMessage(m.Documents.Arena[s.Begin:s.End])
			a.documents[binding] = append(a.documents[binding], doc)
			if !a.sentinelReached && bytes.Contains(doc, []byte(a.sentinelValue)) {
				a.sentinelReached = true
			}
		}
	}
	return nil
}

func (a *testPullAdapter) Context() context.Context     { return a.ctx }
func (a *testPullAdapter) SendMsg(m interface{}) error  { return nil }
func (a *testPullAdapter) RecvMsg(m interface{}) error  { panic("RecvMsg is not supported") }
func (a *testPullAdapter) SendHeader(metadata.MD) error { panic("SendHeader is not supported") }
func (a *testPullAdapter) SetHeader(metadata.MD) error  { panic("SetHeader is not supported") }
func (a *testPullAdapter) SetTrailer(metadata.MD)       { panic("SetTrailer is not supported") }
