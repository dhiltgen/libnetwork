package libnetwork

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/libnetwork/config"
	"github.com/docker/libnetwork/datastore"
	"github.com/docker/libnetwork/driverapi"
	"github.com/docker/libnetwork/etchosts"
	"github.com/docker/libnetwork/netlabel"
	"github.com/docker/libnetwork/options"
	"github.com/docker/libnetwork/types"
)

// A Network represents a logical connectivity zone that containers may
// join using the Link method. A Network is managed by a specific driver.
type Network interface {
	// A user chosen name for this network.
	Name() string

	// A system generated id for this network.
	ID() string

	// The type of network, which corresponds to its managing driver.
	Type() string

	// Create a new endpoint to this network symbolically identified by the
	// specified unique name. The options parameter carry driver specific options.
	// Labels support will be added in the near future.
	CreateEndpoint(name string, options ...EndpointOption) (Endpoint, error)

	// Delete the network.
	Delete() error

	// Endpoints returns the list of Endpoint(s) in this network.
	Endpoints() []Endpoint

	// WalkEndpoints uses the provided function to walk the Endpoints
	WalkEndpoints(walker EndpointWalker)

	// EndpointByName returns the Endpoint which has the passed name. If not found, the error ErrNoSuchEndpoint is returned.
	EndpointByName(name string) (Endpoint, error)

	// EndpointByID returns the Endpoint which has the passed id. If not found, the error ErrNoSuchEndpoint is returned.
	EndpointByID(id string) (Endpoint, error)
}

// EndpointWalker is a client provided function which will be used to walk the Endpoints.
// When the function returns true, the walk will stop.
type EndpointWalker func(ep Endpoint) bool

type svcMap map[string]net.IP

// IpamConf contains all the ipam related configurations for a network
type IpamConf struct {
	PreferredPool string
	SubPool       string
	Options       map[string]string // IPAM input options
	IsV6          bool
	Gateway       string
	AuxAddresses  map[string]string
}

// Validate checks whether the configuration is valid
func (c *IpamConf) Validate() error {
	if c.Gateway != "" && nil == net.ParseIP(c.Gateway) {
		return types.BadRequestErrorf("invalid gateway address %s in IpamConf dtructure", c.Gateway)
	}
	return nil
}

// IpamInfo contains all the ipam related operational info for a network
type IpamInfo struct {
	PoolID string
	Meta   map[string]string
	driverapi.IPAMData
}

// MarshalJSON encodes IpamInfo into json message
func (i *IpamInfo) MarshalJSON() ([]byte, error) {
	m := map[string]interface{}{
		"PoolID": i.PoolID,
	}
	v, err := json.Marshal(&i.IPAMData)
	if err != nil {
		return nil, err
	}
	m["IPAMData"] = string(v)

	if i.Meta != nil {
		m["Meta"] = i.Meta
	}
	return json.Marshal(m)
}

// UnmarshalJSON decodes json message into PoolData
func (i *IpamInfo) UnmarshalJSON(data []byte) error {
	var (
		m   map[string]interface{}
		err error
	)
	if err = json.Unmarshal(data, &m); err != nil {
		return err
	}
	i.PoolID = m["PoolID"].(string)
	if v, ok := m["Meta"]; ok {
		b, _ := json.Marshal(v)
		if err = json.Unmarshal(b, &i.Meta); err != nil {
			return err
		}
	}
	if v, ok := m["IPAMData"]; ok {
		if err = json.Unmarshal([]byte(v.(string)), &i.IPAMData); err != nil {
			return err
		}
	}
	return nil
}

type network struct {
	ctrlr        *controller
	name         string
	networkType  string
	id           string
	ipamType     string
	addrSpace    string
	ipamV4Config []*IpamConf
	ipamV6Config []*IpamConf
	ipamV4Info   []*IpamInfo
	ipamV6Info   []*IpamInfo
	enableIPv6   bool
	endpointCnt  uint64
	generic      options.Generic
	dbIndex      uint64
	svcRecords   svcMap
	dbExists     bool
	persist      bool
	stopWatchCh  chan struct{}
	scope        string
	drvOnce      *sync.Once
	sync.Mutex
}

func (n *network) Name() string {
	n.Lock()
	defer n.Unlock()

	return n.name
}

func (n *network) ID() string {
	n.Lock()
	defer n.Unlock()

	return n.id
}

func (n *network) Type() string {
	n.Lock()
	defer n.Unlock()

	return n.networkType
}

func (n *network) Key() []string {
	n.Lock()
	defer n.Unlock()
	return []string{datastore.NetworkKeyPrefix, n.id}
}

func (n *network) KeyPrefix() []string {
	return []string{datastore.NetworkKeyPrefix}
}

func (n *network) Value() []byte {
	n.Lock()
	defer n.Unlock()
	b, err := json.Marshal(n)
	if err != nil {
		return nil
	}
	return b
}

func (n *network) SetValue(value []byte) error {
	return json.Unmarshal(value, n)
}

func (n *network) Index() uint64 {
	n.Lock()
	defer n.Unlock()
	return n.dbIndex
}

func (n *network) SetIndex(index uint64) {
	n.Lock()
	n.dbIndex = index
	n.dbExists = true
	n.Unlock()
}

func (n *network) Exists() bool {
	n.Lock()
	defer n.Unlock()
	return n.dbExists
}

func (n *network) Skip() bool {
	n.Lock()
	defer n.Unlock()
	return !n.persist
}

func (n *network) New() datastore.KVObject {
	n.Lock()
	defer n.Unlock()

	return &network{
		ctrlr:   n.ctrlr,
		drvOnce: &sync.Once{},
	}
}

// CopyTo deep copies to the destination IpamInfo
func (i *IpamInfo) CopyTo(dstI *IpamInfo) error {
	dstI.PoolID = i.PoolID
	if i.Meta != nil {
		dstI.Meta = make(map[string]string)
		for k, v := range i.Meta {
			dstI.Meta[k] = v
		}
	}

	dstI.AddressSpace = i.AddressSpace
	dstI.Pool = types.GetIPNetCopy(i.Pool)
	dstI.Gateway = types.GetIPNetCopy(i.Gateway)

	if i.AuxAddresses != nil {
		dstI.AuxAddresses = make(map[string]*net.IPNet)
		for k, v := range i.AuxAddresses {
			dstI.AuxAddresses[k] = types.GetIPNetCopy(v)
		}
	}

	return nil
}

func (n *network) CopyTo(o datastore.KVObject) error {
	n.Lock()
	defer n.Unlock()

	dstN := o.(*network)
	dstN.name = n.name
	dstN.id = n.id
	dstN.networkType = n.networkType
	dstN.ipamType = n.ipamType
	dstN.endpointCnt = n.endpointCnt
	dstN.enableIPv6 = n.enableIPv6
	dstN.persist = n.persist
	dstN.dbIndex = n.dbIndex
	dstN.dbExists = n.dbExists
	dstN.drvOnce = n.drvOnce

	for _, v4info := range n.ipamV4Info {
		dstV4Info := &IpamInfo{}
		v4info.CopyTo(dstV4Info)
		dstN.ipamV4Info = append(dstN.ipamV4Info, dstV4Info)
	}

	dstN.generic = options.Generic{}
	for k, v := range n.generic {
		dstN.generic[k] = v
	}

	return nil
}

func (n *network) DataScope() string {
	return n.driverScope()
}

func (n *network) EndpointCnt() uint64 {
	n.Lock()
	defer n.Unlock()
	return n.endpointCnt
}

func (n *network) IncEndpointCnt() error {
	n.Lock()
	n.endpointCnt++
	n.Unlock()

	return n.getController().updateToStore(n)
}

func (n *network) DecEndpointCnt() error {
	n.Lock()
	n.endpointCnt--
	n.Unlock()

	return n.getController().updateToStore(n)
}

// TODO : Can be made much more generic with the help of reflection (but has some golang limitations)
func (n *network) MarshalJSON() ([]byte, error) {
	netMap := make(map[string]interface{})
	netMap["name"] = n.name
	netMap["id"] = n.id
	netMap["networkType"] = n.networkType
	netMap["ipamType"] = n.ipamType
	netMap["addrSpace"] = n.addrSpace
	netMap["endpointCnt"] = n.endpointCnt
	netMap["enableIPv6"] = n.enableIPv6
	if n.generic != nil {
		netMap["generic"] = n.generic
	}
	netMap["persist"] = n.persist
	if len(n.ipamV4Config) > 0 {
		ics, err := json.Marshal(n.ipamV4Config)
		if err != nil {
			return nil, err
		}
		netMap["ipamV4Config"] = string(ics)
	}
	if len(n.ipamV4Info) > 0 {
		iis, err := json.Marshal(n.ipamV4Info)
		if err != nil {
			return nil, err
		}
		netMap["ipamV4Info"] = string(iis)
	}
	if len(n.ipamV6Config) > 0 {
		ics, err := json.Marshal(n.ipamV6Config)
		if err != nil {
			return nil, err
		}
		netMap["ipamV6Config"] = string(ics)
	}
	if len(n.ipamV6Info) > 0 {
		iis, err := json.Marshal(n.ipamV6Info)
		if err != nil {
			return nil, err
		}
		netMap["ipamV6Info"] = string(iis)
	}
	return json.Marshal(netMap)
}

// TODO : Can be made much more generic with the help of reflection (but has some golang limitations)
func (n *network) UnmarshalJSON(b []byte) (err error) {
	var netMap map[string]interface{}
	if err := json.Unmarshal(b, &netMap); err != nil {
		return err
	}
	n.name = netMap["name"].(string)
	n.id = netMap["id"].(string)
	n.ipamType = netMap["ipamType"].(string)
	n.addrSpace = netMap["addrSpace"].(string)
	n.networkType = netMap["networkType"].(string)
	n.endpointCnt = uint64(netMap["endpointCnt"].(float64))
	n.enableIPv6 = netMap["enableIPv6"].(bool)
	if v, ok := netMap["generic"]; ok {
		n.generic = v.(map[string]interface{})
	}
	if v, ok := netMap["persist"]; ok {
		n.persist = v.(bool)
	}
	if v, ok := netMap["ipamV4Config"]; ok {
		if err := json.Unmarshal([]byte(v.(string)), &n.ipamV4Config); err != nil {
			return err
		}
	}
	if v, ok := netMap["ipamV4Info"]; ok {
		if err := json.Unmarshal([]byte(v.(string)), &n.ipamV4Info); err != nil {
			return err
		}
	}
	if v, ok := netMap["ipamV6Config"]; ok {
		if err := json.Unmarshal([]byte(v.(string)), &n.ipamV6Config); err != nil {
			return err
		}
	}
	if v, ok := netMap["ipamV6Info"]; ok {
		if err := json.Unmarshal([]byte(v.(string)), &n.ipamV6Info); err != nil {
			return err
		}
	}
	return nil
}

// NetworkOption is a option setter function type used to pass varios options to
// NewNetwork method. The various setter functions of type NetworkOption are
// provided by libnetwork, they look like NetworkOptionXXXX(...)
type NetworkOption func(n *network)

// NetworkOptionGeneric function returns an option setter for a Generic option defined
// in a Dictionary of Key-Value pair
func NetworkOptionGeneric(generic map[string]interface{}) NetworkOption {
	return func(n *network) {
		n.generic = generic
		if _, ok := generic[netlabel.EnableIPv6]; ok {
			n.enableIPv6 = generic[netlabel.EnableIPv6].(bool)
		}
	}
}

// NetworkOptionPersist returns an option setter to set persistence policy for a network
func NetworkOptionPersist(persist bool) NetworkOption {
	return func(n *network) {
		n.persist = persist
	}
}

// NetworkOptionIpam function returns an option setter for the ipam configuration for this network
func NetworkOptionIpam(ipamDriver string, addrSpace string, ipV4 []*IpamConf, ipV6 []*IpamConf) NetworkOption {
	return func(n *network) {
		n.ipamType = ipamDriver
		n.addrSpace = addrSpace
		n.ipamV4Config = ipV4
		n.ipamV6Config = ipV6
	}
}

func (n *network) processOptions(options ...NetworkOption) {
	for _, opt := range options {
		if opt != nil {
			opt(n)
		}
	}
}

func (n *network) driverScope() string {
	c := n.getController()

	c.Lock()
	// Check if a driver for the specified network type is available
	dd, ok := c.drivers[n.networkType]
	c.Unlock()

	if !ok {
		var err error
		dd, err = c.loadDriver(n.networkType)
		if err != nil {
			// If driver could not be resolved simply return an empty string
			return ""
		}
	}

	return dd.capability.DataScope
}

func (n *network) driver() (driverapi.Driver, error) {
	c := n.getController()

	c.Lock()
	// Check if a driver for the specified network type is available
	dd, ok := c.drivers[n.networkType]
	c.Unlock()

	if !ok {
		var err error
		dd, err = c.loadDriver(n.networkType)
		if err != nil {
			return nil, err
		}
	}

	return dd.driver, nil
}

func (n *network) Delete() error {
	n.Lock()
	c := n.ctrlr
	name := n.name
	id := n.id
	n.Unlock()

	n, err := c.getNetworkFromStore(id)
	if err != nil {
		return &UnknownNetworkError{name: name, id: id}
	}

	numEps := n.EndpointCnt()
	if numEps != 0 {
		return &ActiveEndpointsError{name: n.name, id: n.id}
	}

	if err = n.deleteNetwork(); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if e := c.addNetwork(n); e != nil {
				log.Warnf("failed to rollback deleteNetwork for network %s: %v",
					n.Name(), err)
			}
		}
	}()

	// deleteFromStore performs an atomic delete operation and the
	// network.endpointCnt field will help prevent any possible
	// race between endpoint join and network delete
	if err = n.getController().deleteFromStore(n); err != nil {
		if err == datastore.ErrKeyModified {
			return types.InternalErrorf("operation in progress. delete failed for network %s. Please try again.")
		}
		return err
	}

	defer func() {
		if err != nil {
			n.dbExists = false
			if e := n.getController().updateToStore(n); e != nil {
				log.Warnf("failed to recreate network in store %s : %v", n.name, e)
			}
		}
	}()

	n.ipamRelease()

	return nil
}

func (n *network) deleteNetwork() error {
	d, err := n.driver()
	if err != nil {
		return fmt.Errorf("failed deleting network: %v", err)
	}

	// If it is bridge network type make sure we call the driver about the network
	// because the network may have been created in some past life of libnetwork.
	if n.Type() == "bridge" {
		n.drvOnce.Do(func() {
			err = n.getController().addNetwork(n)
		})
		if err != nil {
			return err
		}
	}

	if err := d.DeleteNetwork(n.ID()); err != nil {
		// Forbidden Errors should be honored
		if _, ok := err.(types.ForbiddenError); ok {
			return err
		}
		log.Warnf("driver error deleting network %s : %v", n.name, err)
	}

	return nil
}

func (n *network) addEndpoint(ep *endpoint) error {
	d, err := n.driver()
	if err != nil {
		return fmt.Errorf("failed to add endpoint: %v", err)
	}

	// If it is bridge network type make sure we call the driver about the network
	// because the network may have been created in some past life of libnetwork.
	if n.Type() == "bridge" {
		n.drvOnce.Do(func() {
			err = n.getController().addNetwork(n)
		})
		if err != nil {
			return err
		}
	}

	err = d.CreateEndpoint(n.id, ep.id, ep.Interface(), ep.generic)
	if err != nil {
		return types.InternalErrorf("failed to create endpoint %s on network %s: %v",
			ep.Name(), n.Name(), err)
	}

	return nil
}

func (n *network) CreateEndpoint(name string, options ...EndpointOption) (Endpoint, error) {
	var err error
	if !config.IsValidName(name) {
		return nil, ErrInvalidName(name)
	}

	if _, err = n.EndpointByName(name); err == nil {
		return nil, types.ForbiddenErrorf("service endpoint with name %s already exists", name)
	}

	ep := &endpoint{name: name, generic: make(map[string]interface{}), iface: &endpointInterface{}}
	ep.id = stringid.GenerateRandomID()

	// Initialize ep.network with a possibly stale copy of n. We need this to get network from
	// store. But once we get it from store we will have the most uptodate copy possible.
	ep.network = n
	ep.network, err = ep.getNetworkFromStore()
	if err != nil {
		return nil, fmt.Errorf("failed to get network during CreateEndpoint: %v", err)
	}
	n = ep.network

	ep.processOptions(options...)

	if err = ep.assignAddress(); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			ep.releaseAddress()
		}
	}()

	if err = n.addEndpoint(ep); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			if e := ep.deleteEndpoint(); e != nil {
				log.Warnf("cleaning up endpoint failed %s : %v", name, e)
			}
		}
	}()

	if err = n.getController().updateToStore(ep); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			if e := n.getController().deleteFromStore(ep); e != nil {
				log.Warnf("error rolling back endpoint %s from store: %v", name, e)
			}
		}
	}()

	// Increment endpoint count to indicate completion of endpoint addition
	if err = n.IncEndpointCnt(); err != nil {
		return nil, err
	}

	return ep, nil
}

func (n *network) Endpoints() []Endpoint {
	var list []Endpoint

	endpoints, err := n.getEndpointsFromStore()
	if err != nil {
		log.Error(err)
	}

	for _, ep := range endpoints {
		list = append(list, ep)
	}

	return list
}

func (n *network) WalkEndpoints(walker EndpointWalker) {
	for _, e := range n.Endpoints() {
		if walker(e) {
			return
		}
	}
}

func (n *network) EndpointByName(name string) (Endpoint, error) {
	if name == "" {
		return nil, ErrInvalidName(name)
	}
	var e Endpoint

	s := func(current Endpoint) bool {
		if current.Name() == name {
			e = current
			return true
		}
		return false
	}

	n.WalkEndpoints(s)

	if e == nil {
		return nil, ErrNoSuchEndpoint(name)
	}

	return e, nil
}

func (n *network) EndpointByID(id string) (Endpoint, error) {
	if id == "" {
		return nil, ErrInvalidID(id)
	}

	ep, err := n.getEndpointFromStore(id)
	if err != nil {
		return nil, ErrNoSuchEndpoint(id)
	}

	return ep, nil
}

func (n *network) updateSvcRecord(ep *endpoint, localEps []*endpoint, isAdd bool) {
	c := n.getController()
	sr, ok := c.svcDb[n.ID()]
	if !ok {
		c.svcDb[n.ID()] = svcMap{}
		sr = c.svcDb[n.ID()]
	}

	n.Lock()
	var recs []etchosts.Record
	if iface := ep.Iface(); iface.Address() != nil {
		if isAdd {
			sr[ep.Name()] = iface.Address().IP
			sr[ep.Name()+"."+n.name] = iface.Address().IP
		} else {
			delete(sr, ep.Name())
			delete(sr, ep.Name()+"."+n.name)
		}

		recs = append(recs, etchosts.Record{
			Hosts: ep.Name(),
			IP:    iface.Address().IP.String(),
		})

		recs = append(recs, etchosts.Record{
			Hosts: ep.Name() + "." + n.name,
			IP:    iface.Address().IP.String(),
		})
	}
	n.Unlock()

	// If there are no records to add or delete then simply return here
	if len(recs) == 0 {
		return
	}

	var sbList []*sandbox
	for _, ep := range localEps {
		if sb, hasSandbox := ep.getSandbox(); hasSandbox {
			sbList = append(sbList, sb)
		}
	}

	for _, sb := range sbList {
		if isAdd {
			sb.addHostsEntries(recs)
		} else {
			sb.deleteHostsEntries(recs)
		}
	}
}

func (n *network) getSvcRecords() []etchosts.Record {
	n.Lock()
	defer n.Unlock()

	var recs []etchosts.Record
	sr, _ := n.ctrlr.svcDb[n.id]

	for h, ip := range sr {
		recs = append(recs, etchosts.Record{
			Hosts: h,
			IP:    ip.String(),
		})
	}

	return recs
}

func (n *network) getController() *controller {
	n.Lock()
	defer n.Unlock()
	return n.ctrlr
}

func (n *network) ipamAllocate() ([]func(), error) {
	var (
		cnl []func()
		err error
	)

	// For now also exclude bridge from using new ipam
	if n.Type() == "host" || n.Type() == "null" || n.Type() == "bridge" {
		return cnl, nil
	}

	ipam, err := n.getController().getIpamDriver(n.ipamType)
	if err != nil {
		return nil, err
	}

	if n.addrSpace == "" {
		if n.addrSpace, err = n.deriveAddressSpace(); err != nil {
			return nil, err
		}
	}

	if n.ipamV4Config == nil {
		n.ipamV4Config = []*IpamConf{&IpamConf{}}
	}

	n.ipamV4Info = make([]*IpamInfo, len(n.ipamV4Config))

	for i, cfg := range n.ipamV4Config {
		if err = cfg.Validate(); err != nil {
			return nil, err
		}
		d := &IpamInfo{}
		n.ipamV4Info[i] = d

		d.PoolID, d.Pool, d.Meta, err = ipam.RequestPool(n.addrSpace, cfg.PreferredPool, cfg.SubPool, cfg.Options, cfg.IsV6)
		if err != nil {
			return nil, err
		}

		defer func() {
			if err != nil {
				if err := ipam.ReleasePool(d.PoolID); err != nil {
					log.Warnf("Failed to release address pool %s after failure to create network %s (%s)", d.PoolID, n.Name(), n.ID())
				}
			}
		}()

		if gws, ok := d.Meta[netlabel.Gateway]; ok {
			if d.Gateway, err = types.ParseCIDR(gws); err != nil {
				return nil, types.BadRequestErrorf("failed to parse gateway address (%v) returned by ipam driver: %v", gws, err)
			}
		}

		// If user requested a specific gateway, libnetwork will allocate it
		// irrespective of whether ipam driver returned a gateway already.
		// If none of the above is true, libnetwork will allocate one.
		if cfg.Gateway != "" || d.Gateway == nil {
			if d.Gateway, _, err = ipam.RequestAddress(d.PoolID, net.ParseIP(cfg.Gateway), nil); err != nil {
				return nil, types.InternalErrorf("failed to allocate gateway (%v): %v", cfg.Gateway, err)
			}
		}

		cnl = append(cnl, func() {
			if err := ipam.ReleaseAddress(d.PoolID, d.Gateway.IP); err != nil {
				log.Warnf("Failed to release gw address %s after failure to create network %s (%s)", d.Gateway, n.Name(), n.ID())
			}
		})
		if cfg.AuxAddresses != nil {
			var ip net.IP
			d.IPAMData.AuxAddresses = make(map[string]*net.IPNet, len(cfg.AuxAddresses))
			for k, v := range cfg.AuxAddresses {
				if ip = net.ParseIP(v); ip == nil {
					return nil, types.BadRequestErrorf("non parsable secondary ip address %s (%s) passed for network %s", k, v, n.Name())
				}
				if d.IPAMData.AuxAddresses[k], _, err = ipam.RequestAddress(d.PoolID, ip, nil); err != nil {
					return nil, types.InternalErrorf("failed to allocate secondary ip address %s(%s): %v", k, v, err)
				}
			}
		}
	}

	return cnl, nil
}

func (n *network) ipamRelease() {
	// For now also exclude bridge from using new ipam
	if n.Type() == "host" || n.Type() == "null" || n.Type() == "bridge" {
		return
	}
	ipam, err := n.getController().getIpamDriver(n.ipamType)
	if err != nil {
		log.Warnf("Failed to retrieve ipam driver to release address pool(s) on delete of network %s (%s): %v", n.Name(), n.ID(), err)
		return
	}
	for _, d := range n.ipamV4Info {
		if d.Gateway != nil {
			if err := ipam.ReleaseAddress(d.PoolID, d.Gateway.IP); err != nil {
				log.Warnf("Failed to release gateway ip address %s on delete of network %s (%s): %v", d.Gateway.IP, n.Name(), n.ID(), err)
			}
		}
		if d.IPAMData.AuxAddresses != nil {
			for k, nw := range d.IPAMData.AuxAddresses {
				if err := ipam.ReleaseAddress(d.PoolID, nw.IP); err != nil {
					log.Warnf("Failed to release secondary ip address %s (%v) on delete of network %s (%s): %v", k, nw.IP, n.Name(), n.ID(), err)
				}
			}
		}
		if err := ipam.ReleasePool(d.PoolID); err != nil {
			log.Warnf("Failed to release address pool %s on delete of network %s (%s): %v", d.PoolID, n.Name(), n.ID(), err)
		}
	}
}

func (n *network) getIPInfo() []*IpamInfo {
	n.Lock()
	defer n.Unlock()
	l := make([]*IpamInfo, 0, len(n.ipamV4Info))
	for _, d := range n.ipamV4Info {
		l = append(l, d)
	}
	return l
}

func (n *network) getIPv4Data() []driverapi.IPAMData {
	l := make([]driverapi.IPAMData, 0, len(n.ipamV4Info))
	n.Lock()
	for _, d := range n.ipamV4Info {
		l = append(l, d.IPAMData)
	}
	n.Unlock()
	return l
}

func (n *network) getIPv6Data() []driverapi.IPAMData {
	l := make([]driverapi.IPAMData, 0, len(n.ipamV6Info))
	n.Lock()
	for _, d := range n.ipamV6Info {
		l = append(l, d.IPAMData)
	}
	n.Unlock()
	return l
}

func (n *network) deriveAddressSpace() (string, error) {
	c := n.getController()
	c.Lock()
	ipd, ok := c.ipamDrivers[n.ipamType]
	c.Unlock()
	if !ok {
		return "", types.NotFoundErrorf("could not find ipam driver %s to get default address space", n.ipamType)
	}
	if n.DataScope() == datastore.GlobalScope {
		return ipd.defaultGlobalAddressSpace, nil
	}
	return ipd.defaultLocalAddressSpace, nil
}
