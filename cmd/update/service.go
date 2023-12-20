package update

// // ServiceCmd represents the run command
// var ServiceCmd = &cobra.Command{
// 	Use:   "service",
// 	Short: "Update an service",

// 	Run: func(cmd *cobra.Command, args []string) {
// 	ctx := context.Background()
//
//provider := wool.New(ctx, configurations.CLI.AsResource())
//
//provider.WithLogger(common.CLI())
//defer provider.Done()
//
//ctx = provider.WithContext(ctx)
// 		service := common.Service(ctx)

// 		_, err := services.Load(ctx, service)
// 		shared.ExitOnError(err, "cannot load service")

// 	},
// }

// func init() {
// 	ServiceCmd.Flags().BoolVarP(&current, "current", "c", false, "update current application")
// }
