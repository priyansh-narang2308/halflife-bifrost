package halflife

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLLMThroughGateway drives the verifier LLM client against a running
// gateway. testenv/verifier-llm.sh starts one and sets:
//
//	HALFLIFE_GW_URL     the gateway's origin
//	HALFLIFE_GW_VK      the verifier key's value (a synthetic test key)
//	HALFLIFE_GW_MODELS  provider/model list, comma-separated; the first is asked
//	HALFLIFE_GW_PROMPT  the user message (the claim and its evidence)
//	HALFLIFE_GW_REASONING  reasoning effort to send, or empty
//	HALFLIFE_GW_OUT     file the outcome is written to, as JSON
//
// It records the outcome rather than asserting it: the script checks what
// each case must show, mock or live.
func TestLLMThroughGateway(t *testing.T) {
	url, out := os.Getenv("HALFLIFE_GW_URL"), os.Getenv("HALFLIFE_GW_OUT")
	if url == "" || out == "" {
		t.Skip("set HALFLIFE_GW_URL and HALFLIFE_GW_OUT (testenv/verifier-llm.sh does)")
	}
	l := NewLLM(url, os.Getenv("HALFLIFE_GW_VK"), LLMOptions{
		Models: strings.Split(os.Getenv("HALFLIFE_GW_MODELS"), ","), MaxTokens: DefaultVerifierMaxTokens,
		ReasoningEffort: os.Getenv("HALFLIFE_GW_REASONING"),
	}, nil)
	messages := []LLMMessage{
		{Role: "system", Content: "You check one claim against the evidence given. Reply with a JSON object: " +
			`"verdict" is SUPPORTED if the evidence states the claim holds now, CONTRADICTED if it states something incompatible, ` +
			`UNVERIFIABLE otherwise; "quote" is the shortest exact sentence from the evidence that decides it, or "" when UNVERIFIABLE.`},
		{Role: "user", Content: os.Getenv("HALFLIFE_GW_PROMPT")},
	}
	start := time.Now()
	var check claimCheck
	res, err := l.Complete(context.Background(), messages, testCheckSchema, &check)
	record := map[string]any{"result": res, "seconds": time.Since(start).Seconds(), "error": nil, "budget_spent": false, "malformed": false, "truncated": false, "check": nil}
	if err != nil {
		var le *LLMError
		record["error"] = err.Error()
		record["budget_spent"] = errors.As(err, &le) && le.BudgetSpent()
		record["malformed"] = errors.Is(err, ErrLLMMalformed)
		record["truncated"] = errors.Is(err, ErrLLMTruncated)
	} else {
		record["check"] = check
	}
	b, _ := json.Marshal(record)
	if werr := os.WriteFile(out, b, 0o600); werr != nil {
		t.Fatal(werr)
	}
	t.Logf("%s", b)
}
