package app

import "fmt"

// CheckConfig loads the integrations file at path as the collector does at
// start and on every reload: the shared loader's checks, then the SQL Server
// and OutSystems probe catalogs. It starts nothing and connects to nothing,
// so the configuration repository's CI can check a rendered file offline.
// It returns the file's version and how many collectors it declares.
func CheckConfig(path string) (version string, collectors int, err error) {
	manager, err := newConfigManager(path)
	if err != nil {
		return "", 0, fmt.Errorf("%s: %w", path, err)
	}
	cfg := manager.Snapshot().Config
	return cfg.Version, len(cfg.Collectors), nil
}
