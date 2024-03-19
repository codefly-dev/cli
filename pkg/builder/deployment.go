package builder

import (
	"fmt"

	"github.com/codefly-dev/core/configurations"
)

func GetNamespace(service *configurations.Service) (string, error) {
	return fmt.Sprintf("%s-%s", service.Project, service.Application), nil
}
