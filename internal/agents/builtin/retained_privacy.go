package builtin

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/agents/retainedprivacy"
	"github.com/wangjohn/agent-archive/internal/agents/retainedprivacy/producer"
)

type fixedRetainedPrivacy struct{}

func (*fixedRetainedPrivacy) transform(request *retainedprivacy.Request) (*retainedprivacy.Proof, error) {
	return producer.Transform(request)
}

// TransformRetainedPrivacy uses only the fixed producer installed by NewBuiltins.
// Custom integration bindings cannot issue retained transform evidence.
func (r *Registry) TransformRetainedPrivacy(request *retainedprivacy.Request) (*retainedprivacy.Proof, error) {
	if r == nil || r.fixedPrivacy == nil {
		return nil, errors.New("retained privacy requires builtin production filters")
	}
	return r.fixedPrivacy.transform(request)
}
