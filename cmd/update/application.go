package update

// // ModuleCmd represents the run command
// var ModuleCmd = &cobra.Command{
// 	Use:   "module",
// 	Short: "Update an module",

// 	Begin: func(cmd *cobra.Command, args []string) {
// 	ctx := context.Background()

//provider := wool.New(ctx, resources.CLI.AsResource())
//
//provider.WithLogger(cli.GetLogger())
//defer provider.Done()
//
//ctx = provider.WithContext(ctx)
// 		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
// 		defer stop()

// 		workspace := common.Workspace(ctx)
// 		config := common.Module(ctx)

// 		golor.Println(`#(blue,bold)[Starting module]: #(italic,white)[{{ .Name }}]
// #(blue,bold)[Ctrl-C anytime to exit...]`, config)

// 		resources.SetMode(resources.ModeModule)
// 		app, err := module.Load(ctx, workspace, config, module.BuilderMode)
// 		cli.ExitOnError(err, "<%s>", config.Name)

// 		err = app.Update(ctx)
// 		cli.ExitOnError(err, "<%s>", config.Name)
// 	},
// }

// var current bool

// func init() {
// 	ModuleCmd.Flags().BoolVarP(&current, "current", "c", false, "update current module")
// 	ModuleCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
// }
