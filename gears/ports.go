package gears

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"github.com/smarterclayton/geard/config"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Port uint

type PortPair struct {
	Internal Port
	External Port
}

type portReservation struct {
	PortPair
	reserved  bool
	allocated bool
	exists    bool
}

type PortPairs []PortPair

func (p PortPairs) ToHeader() string {
	var pairs bytes.Buffer
	for i := range p {
		if i != 0 {
			pairs.WriteString(",")
		}
		pairs.WriteString(strconv.Itoa(int(p[i].Internal)))
		pairs.WriteString("=")
		pairs.WriteString(strconv.Itoa(int(p[i].External)))
	}
	return pairs.String()
}

type portReservations []portReservation

func ReadPortsFromUnitFile(r io.Reader) (PortPairs, error) {
	pairs := make(PortPairs, 0, 4)
	scan := bufio.NewScanner(r)
	for scan.Scan() {
		line := scan.Text()
		if strings.HasPrefix(line, "X-PortMapping=") {
			ports := strings.TrimPrefix(line, "X-PortMapping=")
			var internal int
			var external int
			if _, err := fmt.Sscanf(ports, "%d,%d", &internal, &external); err != nil {
				continue
			}
			if internal > 0 && internal < 65536 && external > 0 && external < 65536 {
				pairs = append(pairs, PortPair{Port(internal), Port(external)})
			}
		}
	}
	if scan.Err() != nil {
		return pairs, scan.Err()
	}
	return pairs, nil
}

func (p PortPairs) WritePortsToUnitFile(w io.Writer) error {
	for i := range p {
		if _, err := fmt.Fprintf(w, "X-PortMapping=%d,%d\n", p[i].Internal, p[i].External); err != nil {
			return err
		}
	}
	return nil
}

// Reserve any unspecified external ports or return an error
// if no ports are available.
func (p PortPairs) reserve() (portReservations, error) {
	reservation := make(portReservations, len(p))
	for i := range p {
		res := &reservation[i]
		res.PortPair = p[i]
	}
	return reservation, nil
}

// Use existing port pairs where possible instead of allocating new ports.
func (p portReservations) reuse(existing PortPairs) ([]Port, error) {
	unreserve := make([]Port, 0, 4)
	for j := range existing {
		ex := &existing[j]
		matched := false
		for i := range p {
			res := &p[i]
			if res.Internal == ex.Internal {
				if res.exists {
					return unreserve, errors.New(fmt.Sprintf("The internal port %d is allocated to more than one external port.", res.Internal))
				}
				if res.External == 0 {
					// Use an already allocated port
					res.External = ex.External
					res.exists = true
				} else if res.External != ex.External {
					unreserve = append(unreserve, ex.External)
				} else {
					res.exists = true
				}
				matched = true
			}
		}
		if !matched {
			unreserve = append(unreserve, ex.External)
		}
	}
	for i := range p {
		res := &p[i]
		if res.External == 0 {
			res.External = allocatePort()
			if res.External == 0 {
				return unreserve, ErrAllocationFailed
			}
			res.reserved = true
		}
	}
	return unreserve, nil
}

// Write reservations to disk or return an error.  Will
// attempt to clean up after a failure by removing partially
// created links.
func (p portReservations) reserve(path string) error {
	var err error
	for i := range p {
		res := &p[i]
		if !res.exists {
			parent, direct := res.External.PortPathsFor()
			os.MkdirAll(parent, 0770)
			err := os.Symlink(path, direct)
			if err != nil {
				log.Printf("ports: Failed to reserve %d, rolling back: %v", res.External, err)
				break
			}
			res.allocated = true
		}
	}

	if err != nil {
		for i := range p {
			res := &p[i]
			if res.allocated {
				_, direct := res.External.PortPathsFor()
				if errr := os.Remove(direct); errr == nil {
					log.Printf("ports: Unable to rollback allocation %d: %v", res.External, err)
					res.allocated = false
				}
			}
		}
		return err
	}

	return nil
}

type device string

func (d device) DevicePath() string {
	return filepath.Join(config.GearBasePath(), "ports", "interfaces", string(d))
}

func (p Port) PortPathsFor() (base string, path string) {
	root := device("1").DevicePath()
	prefix := p / portsPerBlock
	base = filepath.Join(root, strconv.FormatUint(uint64(prefix), 10))
	path = filepath.Join(base, strconv.FormatUint(uint64(p), 10))
	return
}

var ErrAllocationFailed = errors.New("A port could not be allocated.")

func AtomicReserveExternalPorts(path string, ports, existing PortPairs) (PortPairs, error) {
	reservations, errp := ports.reserve()
	if errp != nil {
		return ports, errp
	}
	unreserve, erru := reservations.reuse(existing)
	if erru != nil {
		return ports, erru
	}

	reserved := make(PortPairs, len(reservations))
	for i := range reservations {
		reserved[i] = reservations[i].PortPair
	}

	if err := reservations.reserve(path); err != nil {
		return ports, err
	}

	if len(unreserve) > 0 {
		log.Printf("ports: Releasing %v", unreserve)
	}
	for _, port := range unreserve {
		_, direct := port.PortPathsFor()
		os.Remove(direct) // REPAIR: reserved ports may not be properly released
	}

	return reserved, nil
}

const portsPerBlock = Port(100) // changing this breaks disk structure... don't do it!
const maxReadFailures = 3

//
// Returns 0 if no port can be allocated.  Consumers
// should fail when getting 0 - more ports may become
// available at a later time, but are unlikely to
// come open now.
//
func allocatePort() Port {
	p := <-internalPortAllocator.ports
	log.Printf("ports: Reserved port %d", p)
	return p
}

func StartPortAllocator(min, max Port) {
	internalPortAllocator.min = min
	internalPortAllocator.max = max
	internalPortAllocator.block = uint(min / portsPerBlock)
	go func() {
		internalPortAllocator.findPorts()
		close(internalPortAllocator.ports)
	}()
}

//
// An example of a very simple Port allocator.
//
type portAllocator struct {
	ports    chan Port
	done     chan bool
	block    uint
	failures int
	min      Port
	max      Port
}

var internalPortAllocator = portAllocator{make(chan Port), make(chan bool), 1, 0, 0, 0}

func (p *portAllocator) findPorts() {
	for {
		foundInBlock := 0
		start := Port(p.block) * portsPerBlock
		if start < p.min {
			start = p.min
		}
		end := (Port(p.block) + 1) * portsPerBlock
		if end > p.max {
			end = p.max
			p.block = uint(p.min / portsPerBlock)
		} else {
			p.block += 1
		}
		log.Printf("ports: searching block %d, %d-%d", p.block, start, end-1)

		var taken []string
		parent, _ := start.PortPathsFor()
		f, erro := os.OpenFile(parent, os.O_RDONLY, 0)
		if erro == nil {
			names, errr := f.Readdirnames(int(portsPerBlock))
			f.Close()
			if errr != nil {
				log.Printf("ports: failed to read %s: %v", parent, errr)
				if p.fail() {
					goto finished
				}
				continue
			}
			taken = names
		}

		if reserved := namesToPorts(taken); len(reserved) > 0 {
			existing := reserved[0]
			other := 1
			for n := start; n < end; n++ {
				if existing == n {
					if other < len(reserved) {
						existing = reserved[other]
						other += 1
					}
					continue
				}
				select {
				case p.ports <- n:
					foundInBlock += 1
				case <-p.done:
					goto finished
				}
			}
		} else {
			for n := start; n < end; n++ {
				select {
				case p.ports <- n:
					foundInBlock += 1
				case <-p.done:
					goto finished
				}
			}
		}

		if foundInBlock == 0 {
			log.Printf("ports: failed to find a port between %d-%d ", start, end-1)
			if p.fail() {
				goto finished
			}
		} else {
			p.foundPorts()
		}
	}
finished:
}

func (p *portAllocator) fail() bool {
	p.failures += 1
	if p.failures > maxReadFailures {
		select {
		case p.ports <- 0:
		case <-p.done:
			return true
		}
	}
	return false
}

func (p *portAllocator) foundPorts() {
	p.failures = 0
}

type ports []Port

func (a ports) Len() int           { return len(a) }
func (a ports) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a ports) Less(i, j int) bool { return a[i] < a[j] }

func namesToPorts(reservedNames []string) ports {
	if len(reservedNames) == 0 {
		return ports{}
	}
	reserved := make(ports, len(reservedNames))
	converted := false
	for i := range reservedNames {
		if v, err := strconv.Atoi(reservedNames[i]); err == nil {
			converted = true
			reserved[i] = Port(v)
		}
	}
	if converted {
		sort.Sort(reserved)
		for i := 0; i < len(reserved); i++ {
			if reserved[i] != 0 {
				return reserved[i:]
			}
		}
	}
	return ports{}
}