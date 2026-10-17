package envelope

import "context"

type sessionKey struct{}
type targetSessionKey struct{}
type projectKey struct{}
type modelKey struct{}

// Session IDs scope delivery within an authenticated agent. They do not
// replace node/agent authentication or grant additional permissions.
func ValidSessionID(id string) bool {
	if len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func WithSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionKey{}, id)
}
func Session(ctx context.Context) string { id, _ := ctx.Value(sessionKey{}).(string); return id }
func WithTargetSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, targetSessionKey{}, id)
}
func TargetSession(ctx context.Context) string {
	id, _ := ctx.Value(targetSessionKey{}).(string)
	return id
}

func WithProject(ctx context.Context, project string) context.Context {
	return context.WithValue(ctx, projectKey{}, project)
}
func Project(ctx context.Context) string {
	project, _ := ctx.Value(projectKey{}).(string)
	return project
}

func WithModel(ctx context.Context, model string) context.Context {
	return context.WithValue(ctx, modelKey{}, model)
}
func Model(ctx context.Context) string { model, _ := ctx.Value(modelKey{}).(string); return model }
