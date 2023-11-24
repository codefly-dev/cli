package logs

import (
	corev1 "github.com/codefly-dev/cli/proto/v1/core"
	managementv1 "github.com/codefly-dev/cli/proto/v1/management"
)

type LogManager struct {
	Session *corev1.Session
	LogStorage
}

func (*LogManager) Process(log *managementv1.Log) {

}

type LogStorage interface {
}
