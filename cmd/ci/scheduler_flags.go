package ci

import "github.com/spf13/cobra"

func bindSchedulingFlags(command *cobra.Command) {
	command.Flags().IntVar(&ciJobs, "jobs", 0, "Maximum parallel service tasks (0: auto, capped at 4)")
	command.Flags().BoolVar(&ciFailFast, "fail-fast", true, "Stop scheduling new service tasks after the first failure")
}

func commandScheduleOptions(lockDependencyClosure bool, phase, suite string, reporter *CIReporter) ScheduleOptions {
	return ScheduleOptions{
		Jobs:                  ciJobs,
		FailFast:              ciFailFast,
		LockDependencyClosure: lockDependencyClosure,
		Phase:                 phase,
		Suite:                 suite,
		RuntimeContext:        runtimeContext,
		Reporter:              reporter,
	}
}
