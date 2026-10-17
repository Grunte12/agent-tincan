package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mvanhorn/agent-tincan/internal/client"
	"github.com/mvanhorn/agent-tincan/internal/envelope"
	"os"
	"time"
)

// Codex Core supplies this reserved metadata per tool call. Arguments and
// peer bodies are deliberately excluded; session_id identifies a runtime,
// whereas thread_id is the persistent owner chat.
func codexSession(meta mcp.Meta) (string, error) {
	raw, present := meta["x-codex-turn-metadata"]
	if !present {
		return "", nil
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		return "", errors.New("invalid Codex host metadata")
	}
	id, ok := fields["thread_id"].(string)
	if !ok || id == "" || !envelope.ValidSessionID(id) {
		return "", errors.New("Codex host metadata requires a valid thread_id")
	}
	return id, nil
}

func hostSessionMiddleware(agent string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			failScope := func(err error) (mcp.Result, error) {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil
			}
			ss, ok := req.GetSession().(*mcp.ServerSession)
			if !ok || ss.InitializeParams() == nil || ss.InitializeParams().ClientInfo == nil {
				return next(ctx, method, req)
			}
			nativeHost := HostKind(ss.InitializeParams().ClientInfo.Name)
			host := os.Getenv("TINCAN_HOST_KIND")
			if nativeHost == HostCodex || host == HostCodex {
				id, err := codexSession(req.GetParams().GetMeta())
				if err != nil {
					return failScope(err)
				}
				if id != "" {
					return next(envelope.WithSession(ctx, id), method, req)
				}
				return failScope(errors.New("Codex owner chat ID is unavailable; refusing shared-inbox access"))
			}
			// Only operator-configured hosts require the hook adapter. Legacy
			// Claude channel/other hosts keep their native startup scope.
			if host == "" {
				return next(ctx, method, req)
			}
			if host != "cursor" && host != "antigravity" && host != "muse" {
				return failScope(errors.New("unsupported configured native host"))
			}
			params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if !ok {
				return failScope(errors.New("native host tool parameters unavailable"))
			}
			var scope struct {
				HostContext string `json:"host_context"`
			}
			if json.Unmarshal(params.Arguments, &scope) != nil {
				return failScope(errors.New("invalid native host tool parameters"))
			}
			id, err := hostContextSession(scope.HostContext, os.Getenv("TINCAN_HOST_KEY"), agent, host, time.Now())
			if err != nil {
				return failScope(err)
			}
			if runtime := client.RuntimeSessionID(); runtime != "" && runtime != id {
				return failScope(errors.New("runtime and native hook session identities conflict"))
			}
			ctx = envelope.WithSession(ctx, id)
			return next(ctx, method, req)
		}
	}
}
