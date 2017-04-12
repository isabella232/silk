package lib

import (
	"fmt"
	"net"
	"time"

	"code.cloudfoundry.org/go-db-helpers/db"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/silk/client/config"
	"code.cloudfoundry.org/silk/controller"
	"code.cloudfoundry.org/silk/daemon/vtep"
)

//go:generate counterfeiter -o fakes/database_handler.go --fake-name DatabaseHandler . databaseHandler
type databaseHandler interface {
	Migrate() (int, error)
	AddEntry(*controller.Lease) error
	DeleteEntry(string) error
	LeaseForUnderlayIP(string) (*controller.Lease, error)
	All() ([]controller.Lease, error)
}

//go:generate counterfeiter -o fakes/cidr_pool.go --fake-name CIDRPool . cidrPool
type cidrPool interface {
	GetAvailable([]string) (string, error)
}

//go:generate counterfeiter -o fakes/hardwareAddressGenerator.go --fake-name HardwareAddressGenerator . hardwareAddressGenerator
type hardwareAddressGenerator interface {
	GenerateForVTEP(containerIP net.IP) (net.HardwareAddr, error)
}

type LeaseController struct {
	DatabaseHandler               databaseHandler
	HardwareAddressGenerator      hardwareAddressGenerator
	MaxMigrationAttempts          int
	MigrationAttemptSleepDuration time.Duration
	AcquireSubnetLeaseAttempts    int
	CIDRPool                      cidrPool
	UnderlayIP                    string
	Logger                        lager.Logger
}

func NewLeaseController(cfg config.Config, logger lager.Logger) (*LeaseController, error) {
	sqlDB, err := db.GetConnectionPool(cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %s", err)
	}

	databaseHandler := NewDatabaseHandler(&MigrateAdapter{}, sqlDB)
	leaseController := &LeaseController{
		DatabaseHandler:               databaseHandler,
		HardwareAddressGenerator:      &vtep.HardwareAddressGenerator{},
		MaxMigrationAttempts:          5,
		MigrationAttemptSleepDuration: time.Second,
		AcquireSubnetLeaseAttempts:    10,
		CIDRPool:                      NewCIDRPool(cfg.SubnetRange, cfg.SubnetMask),
		UnderlayIP:                    cfg.UnderlayIP,
		Logger:                        logger,
	}
	if err = leaseController.TryMigrations(); err != nil {
		return nil, fmt.Errorf("migrating database: %s", err)
	}

	return leaseController, nil
}

func (c *LeaseController) TryMigrations() error {
	nErrors := 0
	var err error
	for nErrors < c.MaxMigrationAttempts {
		var n int
		n, err = c.DatabaseHandler.Migrate()
		if err == nil {
			c.Logger.Info("db-migration-complete", lager.Data{"num-applied": n})
			return nil
		}

		nErrors++
		time.Sleep(c.MigrationAttemptSleepDuration)
	}

	return fmt.Errorf("creating table: %s", err)
}

func (c *LeaseController) ReleaseSubnetLease() error {
	err := c.DatabaseHandler.DeleteEntry(c.UnderlayIP)
	if err != nil {
		return fmt.Errorf("releasing lease: %s", err)
	}
	c.Logger.Info("subnet-released", lager.Data{
		"underlay ip": c.UnderlayIP,
	})
	return nil
}

func (c *LeaseController) AcquireSubnetLease(underlayIP string) (*controller.Lease, error) {
	var err error
	var lease *controller.Lease

	lease, err = c.DatabaseHandler.LeaseForUnderlayIP(underlayIP)
	if lease != nil {
		c.Logger.Info("lease-renewed", lager.Data{"lease": lease})
		return lease, nil
	}

	for numErrs := 0; numErrs < c.AcquireSubnetLeaseAttempts; numErrs++ {
		lease, err = c.tryAcquireLease(underlayIP)
		if err == nil {
			c.Logger.Info("lease-acquired", lager.Data{"lease": lease})
			return lease, nil
		}
	}

	return lease, err
}

func (c *LeaseController) RoutableLeases() ([]controller.Lease, error) {
	leases, err := c.DatabaseHandler.All()
	if err != nil {
		return nil, fmt.Errorf("getting all leases: %s", err)
	}

	return leases, nil
}

func (c *LeaseController) tryAcquireLease(underlayIP string) (*controller.Lease, error) {
	var subnet string
	leases, err := c.DatabaseHandler.All()
	if err != nil {
		return nil, fmt.Errorf("getting all subnets: %s", err)
	}
	var taken []string
	for _, lease := range leases {
		taken = append(taken, lease.OverlaySubnet)
	}

	subnet, err = c.CIDRPool.GetAvailable(taken)
	if err != nil {
		return nil, fmt.Errorf("get available subnet: %s", err)
	}

	vtepIP, _, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, fmt.Errorf("parse subnet: %s", err)
	}
	hwAddr, err := c.HardwareAddressGenerator.GenerateForVTEP(vtepIP)
	if err != nil {
		return nil, fmt.Errorf("generate hardware address: %s", err)
	}

	lease := &controller.Lease{
		UnderlayIP:          underlayIP,
		OverlaySubnet:       subnet,
		OverlayHardwareAddr: hwAddr.String(),
	}

	err = c.DatabaseHandler.AddEntry(lease)
	if err != nil {
		return nil, fmt.Errorf("adding lease entry: %s", err)
	}
	return lease, nil
}
