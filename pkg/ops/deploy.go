package ops

//
//func Deploy(ctx context.Context, app *applications.ApplicationOld) error {
//	// Runtime units
//	us, err := app.LoadUnits(ctx)
//	if err != nil {
//		return fmt.Errorf("cannot load units: %w", err)
//	}
//	for _, u := range us {
//		input, err := app.Input(u.Configuration, units.ContainerizationMode)
//		_, err = u.Unit.Expose(input)
//		if err != nil {
//			return fmt.Errorf("cannot configure unit [%s]: %w", u.Configuration.Name, err)
//		}
//		output, err := u.Unit.Deploy(&unit.DeploymentInput{})
//		if err != nil {
//			return fmt.Errorf("cannot deploy unit: %w", err)
//		}
//		fmt.Println("Deployed unit: ", output)
//	}
//	return nil
//}
