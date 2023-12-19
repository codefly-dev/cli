package build

// // ServiceCmd represents the build command
// var ServiceCmd = &cobra.Command{
// 	Use:   "service",
// 	Short: "Build an service",
// 	Run: func(cmd *cobra.Command, args []string) {
// 		ctx := context.Background()
// 		w := wool.Get(ctx).In("build.ServiceCmd")
// 		if len(args) == 0 {
// 			logger.Oops("Please provide a service name")
// 			os.Exit(1)
// 		}

// 		//project := common.Project(ctx)

// 		name := args[0]
// 		reference, err := configurations.ParseServiceReference(name)
// 		cli.ExitOnError(err, "<%s>", name)

// 		var app *configurations.Application
// 		//if reference.Application == "" {
// 		//	app = common.ApplicationConfiguration(true)
// 		//} else {
// 		//	app, err = configurations.LoadApplicationFromName(reference.Application, configurations.WithProject(project))
// 		//	cli.ExitOnError(err, "<%s>", reference.Application)
// 		//}

// 		conf, err := app.LoadServiceFromName(ctx, reference.Name)
// 		cli.ExitOnError(err, "<%s>", name)

// 		instance, err := services.NewServiceInstance(conf, app)
// 		cli.ExitOnError(err, "<%s>", name)

// 		err = instance.SoloBuild(ctx)
// 		cli.ExitOnError(err, "cannot build")

// 		agents.ClearAgents()
// 	},
// }
