package communicate_test

import (
	"fmt"
	"testing"

	"github.com/codefly-dev/cli/pkg/plugins/communicate"
	corev1 "github.com/codefly-dev/cli/proto/v1/core"
)

type seqHandler struct{}

func (s seqHandler) Process(req *corev1.InformationRequest) (*corev1.Answer, error) {
	switch req.Question.Value.(type) {
	case *corev1.Question_Confirm:
		return &corev1.Answer{
			Value: &corev1.Answer_Confirm{
				Confirm: &corev1.ConfirmAnswer{
					Confirmed: false,
				},
			},
		}, nil
	case *corev1.Question_Input:
		return &corev1.Answer{
			Value: &corev1.Answer_Input{
				Input: &corev1.InputAnswer{
					Answer: &corev1.InputAnswer_StringValue{
						StringValue: "working",
					},
				},
			},
		}, nil

	case *corev1.Question_Selection:
		return &corev1.Answer{
			Value: &corev1.Answer_Selection{
				Selection: &corev1.SelectionAnswer{
					Selected: []string{"option 1"},
				},
			},
		}, nil
	}
	return nil, fmt.Errorf("unknown question type: %v", req.Question.Value)
}

var _ communicate.QuestionHandler = &seqHandler{}

func TestSequence(t *testing.T) {
	//logger := shared.NewLogger("communicate_test.TestSequence")
	//logger.SetLevel(shared.DebugLevel)
	//
	//// The client asks for 3 things
	//client := communicate.NewClientContext(communicate.Create, logger)
	//err := client.NewSequence(
	//	client.NewConfirm(&corev1.Message{Name: "confirm"}, true),
	//	client.NewStringInput(&corev1.Message{Name: "input"}, "this is the default value"),
	//	client.NewSelection(&corev1.Message{Name: "selection"},
	//		&corev1.Message{Name: "option 1"},
	//		&corev1.Message{Name: "option 2"},
	//		&corev1.Message{Name: "option 3"}),
	//)
	//assert.NoError(t, err)
	//
	//// The server engage with the client
	//server := communicate.NewServerContext(communicate.Create, logger)
	//server.Handler = &seqHandler{}
	//
	//expectedTypes := []any{new(corev1.Question_Confirm), new(corev1.Question_Input), new(corev1.Question_Selection)}
	//
	//// We will do server -> client until the the server is happy
	//var answer *corev1.Answer
	//for step := 0; ; step++ {
	//	logger.Debugf("step: %v", step)
	//	// Communicate message to send to the client based on previous answer
	//	eng, err := server.Communicate(answer)
	//	assert.NoError(t, err)
	//	request, err := client.Process(eng)
	//	assert.NoError(t, err)
	//	if request == nil {
	//		logger.Debugf("client is done at step %v", step)
	//		break
	//	}
	//	assert.IsType(t, expectedTypes[step], request.Question.Value)
	//	// This is how the server will answer the thing
	//	answer, err = server.Process(request)
	//	assert.NoError(t, err)
	//}
	//
	//// The client state should be complete
	//assert.Equal(t, false, client.Confirm(0).Confirmed)
	//assert.Equal(t, "working", client.Input(1).GetStringValue())
	//assert.Equal(t, []string{"option 1"}, client.Selection(2).Selected)
}
