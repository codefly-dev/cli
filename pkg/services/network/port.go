package network

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/configurations/standards"

	"github.com/codefly-dev/core/wool"
)

func GetAllLocalIPs() ([]string, error) {
	var ips []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}

	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				ips = append(ips, ipnet.IP.String())
			}
		}
	}

	return ips, nil
}

type FixedStrategy struct {
}

func HashInt(s string, low, high int) int {
	hasher := sha256.New()
	hasher.Write([]byte(s))
	hash := hasher.Sum(nil)
	num := binary.BigEndian.Uint32(hash)
	return int(num%uint32(high-low)) + low
}

func APIInt(api string) int {
	switch api {
	case standards.TCP:
		return 0
	case standards.HTTP:
		return 1
	case standards.REST:
		return 2
	case standards.GRPC:
		return 3
	default:
		return 0
	}
}

// ToPort strategy:
// APP-SVC-API
// Between 1100(0) and 4999(9)
// First 11 -> 49: hash app
// Next 0 -> 9: hash svc
// Next 0 - 9: hash name + instance
// Last Digit: API
// 0: TCP
// 1: HTTP/ REST
// 2: gRPC
func ToPort(app string, svc string, name string, api string, instances int) []int {
	appPart := HashInt(app, 11, 49) * 1000
	svcPart := HashInt(svc, 0, 9) * 100
	var ports []int
	for i := 0; i < instances; i++ {
		namePart := HashInt(fmt.Sprintf("%s%d", name, i), 0, 9) * 10
		ports = append(ports, appPart+svcPart+namePart+APIInt(api))
	}
	return ports
}

func IsPortAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

func killProcessUsingPort(port int) error {
	pid, err := getPidUsingPort(port)
	if err != nil {
		return err
	}
	if pid != "" {
		return killProcess(pid)
	}
	return nil
}

func getPidUsingPort(port int) (string, error) {
	cmd := exec.Command("lsof", "-n", fmt.Sprintf("-i4TCP:%d", port))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(output), "\n")
	if len(lines) > 1 {
		fields := strings.Fields(lines[1])
		if len(fields) >= 2 {
			return fields[1], nil // PID is the second field
		}
	}
	return "", nil
}

func killProcess(pid string) error {
	pidInt, _ := strconv.Atoi(pid)
	process, err := os.FindProcess(pidInt)
	if err != nil {
		return err
	} else {
		err = process.Kill()
		if err != nil {
			return err
		}
	}
	return nil
}

func (r FixedStrategy) Reserve(ctx context.Context, host string, endpoints []*ApplicationEndpoint) (*ApplicationEndpointInstances, error) {
	w := wool.Get(ctx).In("FixedStrategy.Reserve")
	m := &ApplicationEndpointInstances{}
	for _, endpoint := range endpoints {
		api, err := configurations.APIAsStandard(endpoint.Endpoint.Api)
		if err != nil {
			return nil, w.Wrapf(err, "cannot get api")
		}
		ports := ToPort(endpoint.Application, endpoint.Service, endpoint.Name, api, int(endpoint.Replicas))
		for _, port := range ports {
			w.Debug("reserving", wool.ApplicationField(endpoint.Application), wool.ServiceField(endpoint.Service), wool.Field("port", port))
			w.Trace("port", wool.ThisField(endpoint), wool.Field("port", port))
			m.ApplicationEndpointInstances = append(m.ApplicationEndpointInstances,
				&ApplicationEndpointInstance{
					ApplicationEndpoint: endpoint,
					Port:                port,
					Host:                host,
				})
		}
	}
	return m, nil
}

// NewServicePortManager manages the ports for a service
func NewServicePortManager(_ context.Context) (*ServiceManager, error) {
	return &ServiceManager{
		strategy: &FixedStrategy{},
		ids:      make(map[string]int),
		host:     "localhost",
	}, nil
}
