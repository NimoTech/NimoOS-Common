// The commmon package provides structs and functions for external code to interact with this gateway service.
package external

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NimoTech/NimoOS-Common/model"
	http2 "github.com/NimoTech/NimoOS-Common/utils/http"
)

const (
	ManagementURLFilename = "management.url"
	StaticURLFilename     = "static.url"
	APIGatewayRoutes      = "/v1/gateway/routes"
	APIGatewayPort        = "/v1/gateway/port"
)

type ManagementService interface {
	CreateRoute(route *model.Route) error
	ChangePort(request *model.ChangePortRequest) error
	GetPort() (error, string)
}

type managementService struct {
	address     string
	addressFile string // set: resolve the address from this file on every call
}

// NewLazyManagementService returns a ManagementService that reads the Gateway's
// address from <runtimePath>/management.url on every call. Unlike
// NewManagementService it never waits, pings or fails at construction, so a
// Gateway that is down or late cannot stop the caller from starting; a call made
// while the Gateway is unreachable returns an error instead.
func NewLazyManagementService(runtimePath string) ManagementService {
	return &managementService{addressFile: filepath.Join(runtimePath, ManagementURLFilename)}
}

func (m *managementService) baseURL() (string, error) {
	address := m.address
	if m.addressFile != "" {
		a, err := getAddress(m.addressFile)
		if err != nil {
			return "", fmt.Errorf("gateway address: %w", err)
		}
		address = strings.TrimSpace(a)
		if address == "" {
			return "", errors.New("gateway address file is empty: " + m.addressFile)
		}
	}
	return strings.TrimSuffix(address, "/"), nil
}

func (m *managementService) CreateRoute(route *model.Route) error {
	base, err := m.baseURL()
	if err != nil {
		return err
	}
	url := base + "/" + strings.TrimPrefix(APIGatewayRoutes, "/")
	body, err := json.Marshal(route)
	if err != nil {
		return err
	}

	response, err := http2.Post(url, body, 30*time.Second)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusCreated {
		return errors.New("failed to create route (status code: " + fmt.Sprint(response.StatusCode) + ")")
	}

	return nil
}

func (m *managementService) ChangePort(request *model.ChangePortRequest) error {
	base, err := m.baseURL()
	if err != nil {
		return err
	}
	url := base + "/" + strings.TrimPrefix(APIGatewayPort, "/")
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}

	response, err := http2.Put(url, body, 30*time.Second)
	if err != nil {
		return err
	}

	if response.StatusCode != http.StatusOK {
		return errors.New("failed to change port (status code: " + fmt.Sprint(response.StatusCode) + ")")
	}

	return nil
}

func (m *managementService) GetPort() (error, string) {
	base, err := m.baseURL()
	if err != nil {
		return err, ""
	}
	url := base + "/" + strings.TrimPrefix(APIGatewayPort, "/")

	response, err := http2.Get(url, 30*time.Second)
	if err != nil {
		return err, ""
	}

	if response.StatusCode != http.StatusOK {
		return errors.New("failed to change port (status code: " + fmt.Sprint(response.StatusCode) + ")"), ""
	}
	str, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return err, ""
	}
	return nil, string(str)
}

func NewManagementService(RuntimePath string) (ManagementService, error) {
	managementAddressFile := filepath.Join(RuntimePath, ManagementURLFilename)

	retry := 10

	for retry > 0 {
		if _, err := os.Stat(managementAddressFile); err == nil {
			break
		}

		fmt.Printf("gateway management address file `%s` not found, retrying in 1 second...(%d)\n", managementAddressFile, retry)

		time.Sleep(1 * time.Second)

		retry--
	}

	address, err := getAddress(managementAddressFile)
	if err != nil {
		return nil, err
	}

	if err := ping(address, 30*time.Second); err != nil {
		return nil, err
	}

	return &managementService{
		address: address,
	}, nil
}
