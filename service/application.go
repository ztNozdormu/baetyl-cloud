package service

import (
	"strings"

	"github.com/baetyl/baetyl-cloud/common"
	"github.com/baetyl/baetyl-cloud/config"
	"github.com/baetyl/baetyl-cloud/models"
	"github.com/baetyl/baetyl-cloud/plugin"
	"github.com/baetyl/baetyl-go/log"
	specV1 "github.com/baetyl/baetyl-go/spec/v1"
)

//go:generate mockgen -destination=../mock/service/application.go -package=plugin github.com/baetyl/baetyl-cloud/service ApplicationService

// ApplicationService ApplicationService
type ApplicationService interface {
	Get(namespace, name, version string) (*specV1.Application, error)
	Create(namespace string, app *specV1.Application) (*specV1.Application, error)
	Update(namespace string, app *specV1.Application) (*specV1.Application, error)
	Delete(namespace, name, version string) error
	List(namespace string, listOptions *models.ListOptions) (*models.ApplicationList, error)
	CreateWithBase(namespace string, app, base *specV1.Application) (*specV1.Application, error)
}

type applicationService struct {
	storage      plugin.ModelStorage
	dbStorage    plugin.DBStorage
	indexService IndexService
}

// NewApplicationService NewApplicationService
func NewApplicationService(config *config.CloudConfig) (ApplicationService, error) {
	ms, err := plugin.GetPlugin(config.Plugin.ModelStorage)
	if err != nil {
		return nil, err
	}
	db, err := plugin.GetPlugin(config.Plugin.DatabaseStorage)
	if err != nil {
		return nil, err
	}
	is, err := NewIndexService(config)
	if err != nil {
		return nil, err
	}
	return &applicationService{
		storage:      ms.(plugin.ModelStorage),
		indexService: is,
		dbStorage:    db.(plugin.DBStorage),
	}, nil
}

// Get get application
func (a *applicationService) Get(namespace, name, version string) (*specV1.Application, error) {
	app, err := a.storage.GetApplication(namespace, name, version)
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil, common.Error(common.ErrResourceNotFound, common.Field("type", "app"),
			common.Field("name", name))
	}

	return app, err
}

// Create create application
func (a *applicationService) Create(namespace string, app *specV1.Application) (*specV1.Application, error) {
	configs, secrets, err := a.getConfigsAndSecrets(namespace, app)
	if err = a.indexService.RefreshConfigIndexByApp(namespace, app.Name, configs); err != nil {
		return nil, err
	}
	if err = a.indexService.RefreshSecretIndexByApp(namespace, app.Name, secrets); err != nil {
		return nil, err
	}

	// create application
	app, err = a.storage.CreateApplication(namespace, app)
	if err != nil {
		return nil, err
	}

	// store application history to db
	if _, err := a.dbStorage.CreateApplication(app); err != nil {
		log.L().Error("store application to db error",
			log.Any("name", app.Name),
			log.Any("namespace", app.Namespace),
			log.Any("version", app.Version),
			log.Error(err))
	}

	return app, nil
}

// Update update application
func (a *applicationService) Update(namespace string, app *specV1.Application) (*specV1.Application, error) {
	err := a.validName(app)
	if err != nil {
		return nil, err
	}

	configs, secrets, err := a.getConfigsAndSecrets(namespace, app)
	if err != nil {
		return nil, err
	}

	newApp, err := a.storage.UpdateApplication(namespace, app)
	if err != nil {
		return nil, err
	}

	if err := a.indexService.RefreshConfigIndexByApp(namespace, newApp.Name, configs); err != nil {
		return nil, err
	}
	if err := a.indexService.RefreshSecretIndexByApp(namespace, newApp.Name, secrets); err != nil {
		return nil, err
	}

	// store app history to db
	if app.Version != newApp.Version {
		if _, err := a.dbStorage.CreateApplication(newApp); err != nil {
			log.L().Error("store application to db error",
				log.Any("name", newApp.Name),
				log.Any("namespace", newApp.Namespace),
				log.Any("version", newApp.Version), log.Error(err))
		}
	}

	return newApp, nil
}

// Delete delete application
func (a *applicationService) Delete(namespace, name, version string) error {
	if err := a.storage.DeleteApplication(namespace, name); err != nil {
		return err
	}

	// TODO: Where dirty data comes from
	if err := a.indexService.RefreshConfigIndexByApp(namespace, name, []string{}); err != nil {
		log.L().Error("Application clean config index error", log.Error(err))
	}
	if err := a.indexService.RefreshSecretIndexByApp(namespace, name, []string{}); err != nil {
		log.L().Error("Application clean secret index error", log.Error(err))
	}

	// mark the application was deleted. err can ignore
	if _, err := a.dbStorage.DeleteApplication(namespace, name, version); err != nil {
		log.L().Error("delete application history error",
			log.Any("name", name),
			log.Any("namespace", namespace),
			log.Any("version", version),
			log.Error(err))
	}
	return nil
}

// List get list config
func (a *applicationService) List(namespace string,
	listOptions *models.ListOptions) (*models.ApplicationList, error) {
	return a.storage.ListApplication(namespace, listOptions)
}

// CreateBaseOther create application with base
func (a *applicationService) CreateWithBase(namespace string, app, base *specV1.Application) (*specV1.Application, error) {
	if base != nil {
		if namespace != base.Namespace {
			err := a.constuctConfig(namespace, base)
			if err != nil {
				return nil, err
			}
		}
		app.Services = append(base.Services, app.Services...)
		app.Volumes = append(base.Volumes, app.Volumes...)
	}

	err := a.validName(app)
	if err != nil {
		return nil, err
	}

	return a.Create(namespace, app)
}

func (a *applicationService) constuctConfig(namespace string, base *specV1.Application) error {
	for _, v := range base.Volumes {
		if v.Config != nil {
			cfg, err := a.storage.GetConfig(base.Namespace, v.Config.Name, "")
			if err != nil {
				log.L().Error("failed to get system config",
					log.Any(common.KeyContextNamespace, base.Namespace),
					log.Any("name", v.Config.Name))
				return common.Error(common.ErrResourceNotFound,
					common.Field("type", "config"),
					common.Field(common.KeyContextNamespace, base.Namespace),
					common.Field("name", v.Config.Name))
			}

			config, err := a.storage.CreateConfig(namespace, cfg)
			if err != nil {
				log.L().Error("failed to create user config",
					log.Any(common.KeyContextNamespace, namespace),
					log.Any("name", v.Config.Name))
				cfg.Name = cfg.Name + "-" + common.RandString(9)
				config, err = a.storage.CreateConfig(namespace, cfg)
				if err != nil {
					return err
				}
				v.Config.Name = config.Name
			}
			v.Config.Version = config.Version
		}
	}
	return nil
}

// get App secrets
func (a *applicationService) getConfigsAndSecrets(namespace string, app *specV1.Application) ([]string, []string, error) {
	var configs []string
	var secrets []string
	for _, vol := range app.Volumes {
		if vol.Config != nil {
			// set the lastest config version
			config, err := a.storage.GetConfig(namespace, vol.Config.Name, "")
			if err != nil {
				return nil, nil, err
			}
			vol.Config.Version = config.Version
			configs = append(configs, vol.Config.Name)
		}
		if vol.Secret != nil {
			secret, err := a.storage.GetSecret(namespace, vol.Secret.Name, "")
			if err != nil {
				return nil, nil, err
			}
			vol.Secret.Version = secret.Version
			secrets = append(secrets, vol.Secret.Name)
		}
	}

	return configs, secrets, nil
}

func (a *applicationService) validName(app *specV1.Application) error {
	sf, vf := make(map[string]bool), make(map[string]bool)
	for _, v := range app.Volumes {
		if _, ok := vf[v.Name]; ok {
			return common.Error(common.ErrAppNameConflict,
				common.Field("where", "Volumes[]"),
				common.Field("name", v.Name))
		}

		vf[v.Name] = true
	}

	for _, s := range app.Services {
		if _, ok := sf[s.Name]; ok {
			return common.Error(common.ErrAppNameConflict,
				common.Field("where", "Services[]"),
				common.Field("name", s.Name))
		}
		for _, vm := range s.VolumeMounts {
			if _, ok := vf[vm.Name]; !ok {
				return common.Error(common.ErrVolumeNotFoundWhenMount,
					common.Field("name", vm.Name))
			}
		}
		sf[s.Name] = true
	}

	return nil
}
