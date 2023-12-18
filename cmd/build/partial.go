package build

// // PartialCmd represents the build command
// var PartialCmd = &cobra.Command{
// 	Use:   "partial",
// 	Short: "Build an partial",
// 	Run: func(cmd *cobra.Command, args []string) {
// 		ctx := shared.NewContext()
// 		w := wool.Get(ctx).In("build.PartialCmd")
// 		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
// 		defer stop()

// 		if len(args) == 0 {
// 			fmt.Println("Please provide a partial name")
// 			os.Exit(0)
// 		}
// 		name := args[0]

// 		project := common.Project(ctx)

// 		conf, err := project.GetPartial(name)
// 		if err != nil {
// 			fmt.Printf("Cannot find partial <%s> in project <%s>\n", name, project.Name)
// 			os.Exit(1)
// 		}
// 		part, err := partial.NewPartial(ctx, project, conf, application.FactoryMode)
// 		shared.UnexpectedExitOnError(err, "<%s>", conf.Name)

// 		err = part.Build(ctx)
// 		shared.UnexpectedExitOnError(err, "cannot build partial")

// 		logger.Debuf("Clearing agents")
// 		agents.ClearAgents()
// 		fmt.Println("Cleaning up...")
// 	},
// }

// func init() {
// 	PartialCmd.Flags().BoolVar(&current, "current", false, "Build the current application")
// }
