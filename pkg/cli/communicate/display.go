package communicate

import (
	"context"
	"fmt"

	agentv1 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v1"

	"github.com/codefly-dev/core/tui"
	"github.com/codefly-dev/golor"
)

func Display(_ context.Context, msg *agentv1.Message, data *agentv1.Display) (*agentv1.Answer, error) {
	fmt.Println(tui.RenderWithMargin(golor.Sprintf(msg.Message, data.Data)))
	return &agentv1.Answer{}, nil
}
