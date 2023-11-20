package communicate

import (
	corev1 "github.com/codefly-dev/cli/proto/v1/core"
)

// Factory

const (
	Create = corev1.Method_CREATE
)

// Runtime

const (
	Sync = corev1.Method_SYNC
)

func (c *ClientContext) Display(msg *corev1.Message, data map[string]string) *corev1.Question {
	return &corev1.Question{
		Method:  c.Method,
		Round:   c.NextRound(),
		Message: msg,
		Value: &corev1.Question_Display{
			Display: &corev1.Display{Data: data},
		},
	}
}

func (c *ClientContext) NewConfirm(msg *corev1.Message, defaultConfirm bool) *corev1.Question {
	return &corev1.Question{
		Method:  c.Method,
		Round:   c.NextRound(),
		Message: msg,
		Value: &corev1.Question_Confirm{
			Confirm: &corev1.Confirm{
				Default: defaultConfirm,
			},
		},
	}
}

func (c *ClientContext) NewStringInput(msg *corev1.Message, defaultValue string) *corev1.Question {
	return &corev1.Question{
		Method:  c.Method,
		Round:   c.NextRound(),
		Message: msg,
		Value: &corev1.Question_Input{
			Input: &corev1.Input{
				Default: &corev1.Input_StringDefault{
					StringDefault: defaultValue,
				},
			},
		},
	}
}

func (c *ClientContext) NewSelection(msg *corev1.Message, options ...*corev1.Message) *corev1.Question {
	return &corev1.Question{
		Method:  c.Method,
		Round:   c.NextRound(),
		Message: msg,
		Value: &corev1.Question_Selection{
			Selection: &corev1.Selection{
				Options: options,
			},
		},
	}
}

func (c *ClientContext) NewChoice(msg *corev1.Message, options ...*corev1.Message) *corev1.Question {
	return &corev1.Question{
		Method:  c.Method,
		Round:   c.NextRound(),
		Message: msg,
		Value: &corev1.Question_Choice{
			Choice: &corev1.Choice{
				Options: options,
			},
		},
	}
}
