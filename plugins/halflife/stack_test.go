package halflife

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/vectorstore"
	"github.com/maximhq/bifrost/plugins/semanticcache"
)

// stack runs Halflife in front of Bifrost's real core and real semantic_cache
// plugin, backed by Bifrost's in-process vector store (chromem) and an
// OpenAI-compatible provider served by httptest. Only the HTTP transport is
// emulated, the same way as in capture_test.go: the transport hooks get their
// own context, and values set there are copied into the core context.
//
// The provider numbers its answers ("answer 1 to: ..."), so a test can tell a
// fresh answer from a cached one.
//
// There is no governance plugin, so requests carry a virtual key ID but no
// grant. The cache scopes entries by the grant's identity, so here tenants
// share entries, as with the cache's identity scoping turned off. Scoping
// itself is checked against the real gateway (testenv/cache-*.sh).
type stack struct {
	t        *testing.T
	hl       *Plugin
	cache    *semanticcache.Plugin
	store    *gatedStore
	client   *bifrost.Bifrost
	provider *fakeProvider
}

type stackOptions struct {
	halflife *Config
}

func newStack(t *testing.T, opts stackOptions) *stack {
	t.Helper()
	ctx := context.Background()
	logger := bifrost.NewNoOpLogger()
	provider := newFakeProvider(t)

	inner, err := vectorstore.NewVectorStore(ctx, &vectorstore.Config{
		Enabled: true, Type: vectorstore.VectorStoreTypeChromem, Config: vectorstore.ChromemConfig{},
	}, logger)
	if err != nil {
		t.Fatalf("vector store: %v", err)
	}
	store := &gatedStore{VectorStore: inner}

	cachePlugin, err := semanticcache.Init(ctx, &semanticcache.Config{
		Provider: schemas.OpenAI, EmbeddingModel: "embed", Dimension: embedDim,
		TTL: time.Minute, Threshold: 0.8, DefaultCacheKey: "test",
	}, logger, store)
	if err != nil {
		t.Fatalf("semantic cache: %v", err)
	}
	cache := cachePlugin.(*semanticcache.Plugin)

	hl, err := Init(ctx, opts.halflife, logger)
	if err != nil {
		t.Fatalf("halflife: %v", err)
	}
	client, err := bifrost.Init(ctx, schemas.BifrostConfig{
		Account: &fakeAccount{baseURL: provider.srv.URL},
		// Halflife first, as the gateway places it (pre_builtin, MinInt).
		LLMPlugins: []schemas.LLMPlugin{hl, cache},
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("bifrost: %v", err)
	}
	cache.SetEmbeddingRequestExecutor(client.EmbeddingRequest)
	t.Cleanup(func() {
		store.open()
		client.Shutdown()
		cache.Cleanup()
		hl.Cleanup()
	})
	return &stack{t: t, hl: hl, cache: cache, store: store, client: client, provider: provider}
}

// reply is what one request returned, as the client sees it.
type reply struct {
	answer  string
	cache   *schemas.BifrostCacheMetadata
	headers map[string]string
}

func (r reply) report() string { return r.headers[HeaderReport] }
func (r reply) status() string { return r.headers[HeaderStatus] }
func (r reply) cacheID() string {
	if r.cache == nil || r.cache.CacheID == nil {
		return ""
	}
	return *r.cache.CacheID
}
func (r reply) hit() string {
	switch {
	case r.cache == nil:
		return "none"
	case !r.cache.CacheHit:
		return "miss"
	case r.cache.HitType != nil:
		return *r.cache.HitType
	}
	return "hit"
}

// ask sends one chat request as tenant vk, through the transport hooks and the
// core pipeline, and returns what the client gets.
func (st *stack) ask(vk, question string) reply {
	st.t.Helper()
	return st.askWith(vk, question, nil)
}

// askWith is ask with extra request context, as the transport sets it from
// x-bf-* headers.
func (st *stack) askWith(vk, question string, set func(*schemas.BifrostContext)) reply {
	st.t.Helper()
	transport := newCtx()
	name := PluginName
	scoped := transport.WithPluginScope(&name)
	if _, err := st.hl.HTTPTransportPreHook(scoped, &schemas.HTTPRequest{Method: "POST", Path: "/v1/chat/completions"}); err != nil {
		st.t.Fatal(err)
	}
	scoped.ReleasePluginScope()

	core := newCtx()
	for k, v := range transport.GetUserValues() {
		core.SetValue(k, v)
	}
	core.SetValue(schemas.BifrostContextKeyRequestID, uuid.NewString())
	core.SetValue(schemas.BifrostContextKeyGovernanceVirtualKeyID, vk)
	if set != nil {
		set(core)
	}
	resp, bErr := st.client.ChatCompletionRequest(core, &schemas.BifrostChatRequest{
		Provider: schemas.OpenAI, Model: "gpt-4o-mini",
		Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr(question)}}},
	})
	status := 200
	var r reply
	if bErr != nil {
		status = 500
	} else {
		r.answer = *resp.Choices[0].Message.Content.ContentStr
		r.cache = resp.ExtraFields.CacheDebug
	}

	scoped = transport.WithPluginScope(&name)
	if err := st.hl.HTTPTransportPostHook(scoped, &schemas.HTTPRequest{}, &schemas.HTTPResponse{StatusCode: status}); err != nil {
		st.t.Fatal(err)
	}
	scoped.ReleasePluginScope()
	meta := &schemas.HTTPResponseMetadata{StatusCode: status, Headers: map[string]string{"Content-Type": "application/json"}}
	scoped = transport.WithPluginScope(&name)
	if err := st.hl.HTTPTransportResponseHeadersHook(scoped, &schemas.HTTPRequest{}, meta); err != nil {
		st.t.Fatal(err)
	}
	scoped.ReleasePluginScope()
	r.headers = meta.Headers
	return r
}

// settle waits for the cache's asynchronous writes.
func (st *stack) settle() { st.cache.WaitForPendingOperations() }

// gatedStore wraps a vector store so a test can hold cache writes (Add) until
// it chooses to release them, to put a write after something else.
type gatedStore struct {
	vectorstore.VectorStore
	mu     sync.Mutex
	gate   chan struct{}
	queued atomic.Int32 // writes waiting at the gate
	stored sync.Map     // entry id -> metadata of every write that reached the store
	// chromem-go reads its document map in Delete without the lock its Add
	// takes (chromem-go v0.7.0, collection.go), so a delete racing a write is a
	// data race in the test store itself. Writes and deletes are serialized
	// here; Redis, the gateway's store, needs no such help.
	writeMu sync.Mutex
}

func (g *gatedStore) Delete(ctx context.Context, namespace, id string) error {
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	return g.VectorStore.Delete(ctx, namespace, id)
}

// expiresIn is how far past now the stored entry id expires.
func (g *gatedStore) expiresIn(t *testing.T, id string) time.Duration {
	t.Helper()
	m, ok := g.stored.Load(id)
	if !ok {
		t.Fatalf("entry %s was never written", id)
	}
	var at int64
	switch v := m.(map[string]any)["expires_at"].(type) {
	case int64:
		at = v
	case int:
		at = int64(v)
	case float64:
		at = int64(v)
	default:
		t.Fatalf("expires_at = %T %v", v, v)
	}
	return time.Until(time.Unix(at, 0))
}

// hold makes every later Add wait until open is called.
func (g *gatedStore) hold() {
	g.mu.Lock()
	g.gate = make(chan struct{})
	g.mu.Unlock()
}

func (g *gatedStore) open() {
	g.mu.Lock()
	if g.gate != nil {
		close(g.gate)
		g.gate = nil
	}
	g.mu.Unlock()
}

// waitQueued blocks until n writes are waiting at the gate.
func (g *gatedStore) waitQueued(t *testing.T, n int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for g.queued.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d cache writes queued, want %d", g.queued.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func (g *gatedStore) Add(ctx context.Context, namespace, id string, embedding []float32, metadata map[string]any) error {
	g.mu.Lock()
	gate := g.gate
	g.mu.Unlock()
	if gate != nil {
		g.queued.Add(1)
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	g.stored.Store(id, metadata)
	return g.VectorStore.Add(ctx, namespace, id, embedding, metadata)
}

// fakeProvider is an OpenAI-compatible chat and embeddings endpoint.
type fakeProvider struct {
	srv   *httptest.Server
	chats atomic.Int32
}

const embedDim = 32

func newFakeProvider(t *testing.T) *fakeProvider {
	p := &fakeProvider{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		n := p.chats.Add(1)
		q := req.Messages[len(req.Messages)-1].Content
		writeJSONResponse(w, map[string]any{
			"id": fmt.Sprintf("chatcmpl-%d", n), "object": "chat.completion", "created": 1793491200, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": fmt.Sprintf("answer %d to: %s", n, q)}}},
			"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 5, "total_tokens": 10},
		})
	})
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string          `json:"model"`
			Input json.RawMessage `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var inputs []string
		var one string
		if json.Unmarshal(req.Input, &one) == nil {
			inputs = []string{one}
		} else {
			json.Unmarshal(req.Input, &inputs)
		}
		data := make([]any, len(inputs))
		for i, in := range inputs {
			data[i] = map[string]any{"object": "embedding", "index": i, "embedding": embed(in)}
		}
		writeJSONResponse(w, map[string]any{"object": "list", "data": data, "model": req.Model,
			"usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1}})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// embed is a hashed bag of words (lower-cased, letters and digits), as in
// testenv/mockllm: texts sharing most words are close.
func embed(text string) []float64 {
	v := make([]float64, embedDim)
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		h := fnv.New32a()
		h.Write([]byte(w))
		v[h.Sum32()%embedDim]++
	}
	var n float64
	for _, x := range v {
		n += x * x
	}
	if n == 0 {
		v[0], n = 1, 1
	}
	for i := range v {
		v[i] /= math.Sqrt(n)
	}
	return v
}

type fakeAccount struct{ baseURL string }

func (a *fakeAccount) GetConfiguredProviders() ([]schemas.ModelProvider, error) {
	return []schemas.ModelProvider{schemas.OpenAI}, nil
}

func (a *fakeAccount) GetKeysForProvider(context.Context, schemas.ModelProvider) ([]schemas.Key, error) {
	return []schemas.Key{{ID: "test", Value: *schemas.NewSecretVar("sk-test"), Models: schemas.WhiteList{"*"}, Weight: 1}}, nil
}

func (a *fakeAccount) GetConfigForProvider(schemas.ModelProvider) (*schemas.ProviderConfig, error) {
	return &schemas.ProviderConfig{
		NetworkConfig:            schemas.NetworkConfig{BaseURL: a.baseURL, DefaultRequestTimeoutInSeconds: 10},
		ConcurrencyAndBufferSize: schemas.ConcurrencyAndBufferSize{Concurrency: 16, BufferSize: 256},
	}, nil
}

// The harness itself: a miss reaches the provider, a repeat is a direct hit.
func TestStackServesFromCache(t *testing.T) {
	st := newStack(t, stackOptions{})
	first := st.ask("vk-a", "What is the price of the Acme Pro plan?")
	st.settle()
	again := st.ask("vk-a", "What is the price of the Acme Pro plan?")
	if first.hit() != "miss" || again.hit() != "direct" || again.answer != first.answer || st.provider.chats.Load() != 1 {
		t.Fatalf("first=%s %q, again=%s %q, provider calls=%d", first.hit(), first.answer, again.hit(), again.answer, st.provider.chats.Load())
	}
	if first.cacheID() == "" || again.cacheID() != first.cacheID() {
		t.Errorf("cache ids %q %q", first.cacheID(), again.cacheID())
	}
}
