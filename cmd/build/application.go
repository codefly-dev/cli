package build

// // ApplicationCmd represents the build command
// var ApplicationCmd = &cobra.Command{
// 	Use:   "application",
// 	Short: "Build an application",
// 	Run: func(cmd *cobra.Command, args []string) {
// 	ctx := context.Background()

//provider := wool.New(ctx, configurations.CLI.AsResource())
//
//provider.WithLogger(common.CLI())
//defer provider.Done()
//
//ctx = provider.WithContext(ctx)
// 		config := common.Application(ctx)
// 		project := common.Project(ctx)

// 		golor.Println(`#(blue,bold)[Building applications]: #(italic,white)[{{ .Name }}]`, config)
// 		configurations.SetMode(configurations.ModeApplication)

// 		app, err := application.Load(ctx, project, config, application.FactoryMode)
// 		cli.ExitOnError(err, "<%s>", config.Name)

// 		err = app.FactoryInit(ctx)
// 		cli.ExitOnError(err, "<%s>", config.Name)

// 		err = app.Build(ctx)
// 		cli.ExitOnError(err, "<%s>", config.Name)

// 		agents.ClearAgents()
// 	},
// }

// var current bool

// func init() {
// 	ApplicationCmd.Flags().BoolVarP(&current, "current", "c", false, "Use the current application")
// }
