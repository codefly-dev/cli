package ops

//
//func Validate(ctx context.Context, app *applications.ApplicationOld) error {
//	// We will validate the Flavor dependencies
//	us, err := app.LoadUnits(ctx)
//	if err != nil {
//		return fmt.Errorf("cannot load units: %w", err)
//	}
//	for _, u := range us {
//		input, err := app.Default(u.Configuration, units.ContainerizationMode)
//		_, err = u.Unit.Expose(input)
//		if err != nil {
//			return fmt.Errorf("cannot configure unit [%s]: %w", u.Configuration.Name, err)
//		}
//		output, err := u.Unit.Info(&unit.InfoInput{})
//		if err != nil {
//			return fmt.Errorf("cannot get info unit: %w", err)
//		}
//		fmt.Println("Info: ", output)
//	}
//	return nil
//}
