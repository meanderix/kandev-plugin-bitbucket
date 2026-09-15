package plugin

import (
	"context"
	"fmt"
	"github.com/kandev/kandev/pkg/pluginsdk"
)

func (*taskReader) Move(context.Context, pluginsdk.MoveTaskInput) (*pluginsdk.MoveTaskOutcome, error) {
	return nil, fmt.Errorf("unexpected task move")
}
func (*associationTaskReader) Move(context.Context, pluginsdk.MoveTaskInput) (*pluginsdk.MoveTaskOutcome, error) {
	return nil, fmt.Errorf("unexpected task move")
}
