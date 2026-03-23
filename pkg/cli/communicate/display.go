package communicate

import (
	"context"
	"fmt"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"

	"github.com/codefly-dev/core/tui"
	"github.com/codefly-dev/golor"
)

func Display(ctx context.Context, msg *agentv0.Message, data *agentv0.Display) (*agentv0.Answer, error) {
	fmt.Println(tui.RenderWithMargin(golor.Sprintf(msg.Message, data.Data)))
	return &agentv0.Answer{}, nil
}
