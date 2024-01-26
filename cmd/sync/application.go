package sync

// // ApplicationCmd represents the run command
// var ApplicationCmd = &cobra.Command{
// 	Use:   "application",
// 	Short: "Sync an application",

// 	Start: func(cmd *cobra.Command, args []string) {

// 		ctx := context.Background()
// 		project := common.Project(ctx)

// 		var config *configurations.Application
// 		//var err error
// 		//// Optional application argument
// 		//if len(args) > 0 {
// 		//	name := args[0]
// 		//	config, err = configurations.LoadApplicationFromName(name, configurations.WithProject(project))
// 		//	shared.ExitOnError(err, "cannot load application <%s>", name)
// 		//} else {
// 		//	config = common.ApplicationConfiguration(current)
// 		//}

// 		configurations.SetMode(configurations.ModeApplication)
// 		app, err := application.Load(ctx, project, config, application.BuilderMode)

// 		cli.ExitOnError(err, "<%s>", config.Name)

// 		//others, err := project.OtherApplications(config)
// 		//cli.ExitOnError(err, "cannot get other applications")
// 		//for _, other := range others {
// 		//	logger.Debuf("not loading other application -- this is what partial are for: %v", other.Name)
// 		//	// otherApp, err := application.Load(other, project)
// 		//	// cli.ExitOnError(err, "<%s>", other.Name)
// 		//	// app.AddDependency(otherApp)
// 		//}
// 		//
// 		//logger.Debuf("other applications: %d", len(others))
// 		//golor.Println(`#(blue,bold)[Syncing application]: #(italic,white)[{{.Configuration.Name}}]`, app)

// 		err = app.Sync(ctx)
// 		cli.ExitOnError(err, "cannot sync application")
// 	},
// }

// func init() {
// 	ApplicationCmd.Flags().BoolVar(&current, "current", false, "Start the current application")
// }
