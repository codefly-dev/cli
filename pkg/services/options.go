package services

//
//type Option[T any] struct {
//	Name        string
//	Description string
//	Value       *T
//	Options     T
//}
//
//func ExtractEndpoint(opt string) (string, error) {
//	// Of the form  guestbook-go.redis::read::tcp -> app.service::endpoint::protocol
//	tokens := strings.SplitN(opt, ".", 2)
//	if len(tokens) != 2 {
//		return "", fmt.Errorf("cannot extract endpoint from %s, getting rid of app", opt)
//	}
//	tokens = strings.Split(tokens[1], "::")
//	if len(tokens) != 3 {
//		return "", fmt.Errorf("cannot extract endpoint from %s, getting rid of protocol", opt)
//	}
//	return fmt.Sprintf("%s::%s", tokens[0], tokens[1]), nil
//}
//
//func (opt Option[T]) Ask() (any, error) {
//	var t T
//	switch any(t).(type) {
//	case *basev1.Choice:
//		choice, ok := any(opt.Options).(*basev1.Choice)
//		if !ok {
//			return nil, errors.Errorf("cannot convert default value to choice")
//		}
//		var options []string
//		data := make(map[string]string)
//		for _, option := range choice.Options {
//			p, err := ExtractEndpoint(option)
//			if err != nil {
//				return nil, err
//			}
//			data[p] = option
//			options = append(options, p)
//		}
//		prompt := &survey.Select{
//			Message: opt.Description,
//			Options: options,
//		}
//		var selected string
//		err := survey.AskOne(prompt, &selected)
//		if err != nil {
//			return nil, errors.Wrapf(err, "cannot ask for option %s", opt.Description)
//		}
//		return selected, nil
//	default:
//		prompt := &survey.Options{
//			Message: opt.Description,
//			Options: fmt.Sprintf("%v", opt.Options),
//		}
//		err := survey.AskOne(prompt, opt.Value)
//		if err != nil {
//			return nil, errors.Wrapf(err, "cannot ask for option %s", opt.Description)
//		}
//		return *opt.Value, nil
//	}
//}
//
//func (opt Option[T]) OptionName() string {
//	return opt.Name
//}
//
//type Prompt struct {
//	opts    []OptionArgument
//	results map[string]any
//}
//
//type OptionArgument interface {
//	OptionName() string
//	Ask() (any, error)
//}
//
//func (p *Prompt) With(opts ...*basev1.Option) error {
//	for _, opt := range opts {
//		conv, err := ConvertToOption(opt)
//		if err != nil {
//			return err
//		}
//		p.opts = append(p.opts, conv.(OptionArgument))
//	}
//	return nil
//}
//
//func (p *Prompt) Ask() error {
//	for _, opt := range p.opts {
//		value, err := opt.Ask()
//		if err != nil {
//			return err
//		}
//		p.results[opt.OptionName()] = value
//
//	}
//	return nil
//}
//
//func (p *Prompt) ToSpec() ([]byte, error) {
//	content, err := yaml.Marshal(p.results)
//	if err != nil {
//		return nil, err
//	}
//	return content, nil
//}
//
//func NewPrompt() *Prompt {
//	return &Prompt{results: make(map[string]any)}
//}
//
//func NewRuntimeOption[T any](name string, description string, value T) *basev1.Option {
//	return &basev1.Option{
//		Name:        name,
//		Description: description,
//		Options:     NewRuntimeOptionDefault(value),
//	}
//}
//
///*
//
//Conversion
//
//*/
//
//func NewRuntimeOptionDefault[T any](value T) *basev1.OptionDefault {
//	switch v := any(value).(type) {
//	case string:
//		return &basev1.OptionDefault{Value: &basev1.OptionDefault_StrValue{StrValue: v}}
//	case float32:
//		return &basev1.OptionDefault{Value: &basev1.OptionDefault_FloatValue{FloatValue: v}}
//	case bool:
//		return &basev1.OptionDefault{Value: &basev1.OptionDefault_BoolValue{BoolValue: v}}
//	case int:
//		return &basev1.OptionDefault{Value: &basev1.OptionDefault_IntValue{IntValue: int64(v)}}
//	case *basev1.Choice:
//		return &basev1.OptionDefault{Value: &basev1.OptionDefault_ChoiceValue{ChoiceValue: v}}
//	default:
//		return nil
//	}
//}
//
//func ConvertToOption(opt *basev1.Option) (interface{}, error) {
//	logger := shared.GetBaseLogger(ctx).With("agents.ConvertToOption")
//	if stringValue := opt.Options.GetStrValue(); stringValue != "" {
//		return &Option[string]{
//			Name:        opt.Name,
//			Description: opt.Description,
//			Value:       &stringValue,
//			Options:     opt.Options.GetStrValue(),
//		}, nil
//	}
//	if floatValue := opt.Options.GetFloatValue(); floatValue != 0 {
//		return &Option[float32]{
//			Name:        opt.Name,
//			Description: opt.Description,
//			Value:       &floatValue,
//			Options:     opt.Options.GetFloatValue(),
//		}, nil
//	}
//	if boolValue := opt.Options.GetBoolValue(); boolValue {
//		return &Option[bool]{
//			Name:        opt.Name,
//			Description: opt.Description,
//			Value:       &boolValue,
//			Options:     opt.Options.GetBoolValue(),
//		}, nil
//	}
//	if intValue := opt.Options.GetIntValue(); intValue != 0 {
//		value := int(intValue)
//		return &Option[int]{
//			Name:        opt.Name,
//			Description: opt.Description,
//			Value:       &value,
//			Options:     int(opt.Options.GetIntValue()),
//		}, nil
//	}
//	if choiceValue := opt.Options.GetChoiceValue(); choiceValue != nil {
//		return &Option[*basev1.Choice]{
//			Name:        opt.Name,
//			Description: opt.Description,
//			Value:       &choiceValue,
//			Options:     choiceValue,
//		}, nil
//	}
//	return nil, logger.Errorf("unknown type in factory option argument")
//}
