package ops

//
//func Containerize(ctx context.Context, app *applications.ApplicationOld) error {
//	// Runtime units
//	us, err := app.LoadUnits(ctx)
//	if err != nil {
//		return fmt.Errorf("cannot load units: %w", err)
//	}
//	for _, u := range us {
//		input, err := app.Options(u.Configuration, units.ContainerizationMode)
//		_, err = u.Unit.Expose(input)
//		if err != nil {
//			return fmt.Errorf("cannot configure unit [%s]: %w", u.Configuration.Name, err)
//		}
//		_, err = u.Unit.Containerize(&unit.ContainerizeInput{})
//		if err != nil {
//			return fmt.Errorf("cannot containerize unit: %w", err)
//		}
//	}
//	return nil
//}
