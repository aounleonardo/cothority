package service

/*
The service.go defines what to do for each API-call. This part of the service
runs on the node.
*/

import (
	"errors"

	"bytes"

	"gopkg.in/dedis/onet.v1"
	"gopkg.in/dedis/onet.v1/crypto"
	"gopkg.in/dedis/onet.v1/log"
	"gopkg.in/dedis/onet.v1/network"

	"sync"
	"time"

	"math/rand"

	"github.com/dedis/onchain-secrets"
	"github.com/dedis/onchain-secrets/protocol"
	"github.com/dedis/protobuf"
	"gopkg.in/dedis/cothority.v1/messaging"
	"gopkg.in/dedis/cothority.v1/skipchain"
	"gopkg.in/dedis/crypto.v0/share"
)

// Used for tests
var templateID onet.ServiceID

const propagationTimeout = 10000

func init() {
	network.RegisterMessage(Storage{})
	var err error
	templateID, err = onet.RegisterNewService(ocs.ServiceName, newService)
	log.ErrFatal(err)
}

// Service holds all data for the ocs-service
type Service struct {
	// We need to embed the ServiceProcessor, so that incoming messages
	// are correctly handled.
	*onet.ServiceProcessor

	propagateOCS messaging.PropagationFunc

	Storage   *Storage
	saveMutex sync.Mutex
}

// Storage holds the skipblock-bunches for the OCS-skipchain.
type Storage struct {
	OCSs     *ocs.SBBStorage
	Accounts map[string]*ocs.Darc
	Shared   map[string]*protocol.SharedSecret
}

// CreateSkipchains sets up a new OCS-skipchain.
func (s *Service) CreateSkipchains(req *ocs.CreateSkipchainsRequest) (reply *ocs.CreateSkipchainsReply,
	cerr onet.ClientError) {

	// Create OCS-skipchian
	c := skipchain.NewClient()
	reply = &ocs.CreateSkipchainsReply{}

	log.Lvl2("Creating OCS-skipchain")
	reply.OCS, cerr = c.CreateGenesis(req.Roster, 1, 1, ocs.VerificationOCS, nil, nil)
	if cerr != nil {
		return nil, cerr
	}
	replies, err := s.propagateOCS(req.Roster, reply.OCS, propagationTimeout)
	if err != nil {
		return nil, onet.NewClientErrorCode(ocs.ErrorProtocol, err.Error())
	}
	if replies != len(req.Roster.List) {
		log.Warn("Got only", replies, "replies for ocs-propagation")
	}

	// Do DKG on the nodes
	tree := req.Roster.GenerateNaryTreeWithRoot(len(req.Roster.List), s.ServerIdentity())
	pi, err := s.CreateProtocol(protocol.NameDKG, tree)
	setupDKG := pi.(*protocol.SetupDKG)
	setupDKG.Wait = true
	setupDKG.SetConfig(&onet.GenericConfig{Data: reply.OCS.Hash})
	//log.Print(s.ServerIdentity(), reply.OCS.Hash)
	if err := pi.Start(); err != nil {
		return nil, onet.NewClientErrorCode(ocs.ErrorProtocol, err.Error())
	}
	log.Lvl3("Started DKG-protocol - waiting for done")
	select {
	case <-setupDKG.Done:
		shared, err := setupDKG.SharedSecret()
		if err != nil {
			return nil, onet.NewClientErrorCode(ocs.ErrorProtocol, err.Error())
		}
		s.saveMutex.Lock()
		s.Storage.Shared[string(reply.OCS.Hash)] = shared
		s.saveMutex.Unlock()
		reply.X = shared.X
	case <-time.After(propagationTimeout * time.Millisecond):
		return nil, onet.NewClientErrorCode(ocs.ErrorProtocol,
			"dkg didn't finish in time")
	}

	return
}

// EditDarc adds a new account or modifies an existing one.
func (s *Service) EditDarc(req *ocs.EditDarcRequest) (reply *ocs.EditDarcReply,
	cerr onet.ClientError) {
	if _, exists := s.Storage.Accounts[string(req.Darc.ID)]; exists {
		log.Lvl2("Modifying existing account")
	} else {
		log.Lvl2("Adding new account")
	}
	dataOCS := &ocs.DataOCS{
		Readers: req.Darc,
	}
	s.saveMutex.Lock()
	ocsBunch := s.Storage.OCSs.GetBunch(req.OCS)
	s.saveMutex.Unlock()
	data, err := protobuf.Encode(dataOCS)
	if err != nil {
		return nil, onet.NewClientErrorCode(ocs.ErrorParameter, err.Error())
	}
	sb, cerr := s.BunchAddBlock(ocsBunch, ocsBunch.Latest.Roster, data)
	if cerr != nil {
		return
	}
	s.Storage.Accounts[string(req.Darc.ID)] = req.Darc
	s.save()
	return &ocs.EditDarcReply{SB: sb}, nil
}

// ReadDarc returns the latest valid Darc given its identity.
func (s *Service) ReadDarc(req *ocs.ReadDarcRequest) (reply *ocs.ReadDarcReply,
	cerr onet.ClientError) {
	log.Lvl2("Reading darc", req.DarcID, req.Recursive)
	darc, exists := s.Storage.Accounts[string(req.DarcID)]
	if !exists {
		return nil, onet.NewClientErrorCode(ocs.ErrorParameter, "this Darc doesn't exist")
	}
	darcs := map[string]*ocs.Darc{}
	if req.Recursive {
		s.darcRecursive(darcs, darc.ID)
	}
	delete(darcs, string(darc.ID))
	reply = &ocs.ReadDarcReply{Darc: []*ocs.Darc{darc}}
	for _, d := range darcs {
		reply.Darc = append(reply.Darc, d)
	}
	return
}

// WriteRequest adds a block the OCS-skipchain with a new file.
func (s *Service) WriteRequest(req *ocs.WriteRequest) (reply *ocs.WriteReply,
	cerr onet.ClientError) {
	log.Lvl2("Write request")
	reply = &ocs.WriteReply{}
	s.saveMutex.Lock()
	ocsBunch := s.Storage.OCSs.GetBunch(req.OCS)
	s.saveMutex.Unlock()
	if ocsBunch == nil {
		return nil, onet.NewClientErrorCode(ocs.ErrorParameter, "didn't find that bunch")
	}
	block := ocsBunch.GetByID(req.OCS)
	if block == nil {
		log.Error("not")
		return nil, onet.NewClientErrorCode(ocs.ErrorParameter, "Didn't find block-skipchain")
	}
	dataOCS := &ocs.DataOCS{
		Write:   req.Write,
		Readers: req.Readers,
	}
	data, err := protobuf.Encode(dataOCS)
	if err != nil {
		return nil, onet.NewClientErrorCode(ocs.ErrorParameter, err.Error())
	}

	i := 1
	for {
		reply.SB, cerr = s.BunchAddBlock(ocsBunch, block.Roster, data)
		if cerr == nil {
			break
		}
		if cerr.ErrorCode() == skipchain.ErrorBlockInProgress {
			log.Lvl2("Waiting for block to be propagated...")
			time.Sleep(time.Duration(rand.Intn(20)*i) * time.Millisecond)
			i++
		} else {
			return nil, cerr
		}
	}

	log.Lvl2("Writing a key to the skipchain")
	if cerr != nil {
		log.Error(cerr)
		return
	}

	replies, err := s.propagateOCS(ocsBunch.Latest.Roster, reply.SB, propagationTimeout)
	if err != nil {
		cerr = onet.NewClientErrorCode(ocs.ErrorProtocol, err.Error())
		return
	}
	if replies != len(ocsBunch.Latest.Roster.List) {
		log.Warn("Got only", replies, "replies for write-propagation")
	}
	return
}

// ReadRequest asks for a read-offer on the skipchain for a reader on a file.
func (s *Service) ReadRequest(req *ocs.ReadRequest) (reply *ocs.ReadReply,
	cerr onet.ClientError) {
	log.Lvl2("Requesting a file. Reader:", req.Read.Public)
	reply = &ocs.ReadReply{}
	s.saveMutex.Lock()
	ocsBunch := s.Storage.OCSs.GetBunch(req.OCS)
	s.saveMutex.Unlock()
	if ocsBunch == nil {
		return nil, onet.NewClientErrorCode(ocs.ErrorParameter, "didn't find that block")
	}
	block := ocsBunch.GetByID(req.OCS)
	if block == nil {
		return nil, onet.NewClientErrorCode(ocs.ErrorParameter, "Didn't find block-skipchain")
	}
	dataOCS := &ocs.DataOCS{
		Read: req.Read,
	}
	data, err := protobuf.Encode(dataOCS)
	if err != nil {
		return nil, onet.NewClientErrorCode(ocs.ErrorParameter, err.Error())
	}

	i := 1
	for {
		reply.SB, cerr = s.BunchAddBlock(ocsBunch, block.Roster, data)
		if cerr == nil {
			break
		}
		if cerr.ErrorCode() == skipchain.ErrorBlockInProgress {
			log.Lvl2("Waiting for block to be propagated...")
			time.Sleep(time.Duration(rand.Intn(20)*i) * time.Millisecond)
			i++
		} else {
			return nil, cerr
		}
	}

	replies, err := s.propagateOCS(ocsBunch.Latest.Roster, reply.SB, propagationTimeout)
	if err != nil {
		cerr = onet.NewClientErrorCode(ocs.ErrorProtocol, err.Error())
		return
	}
	if replies != len(ocsBunch.Latest.Roster.List) {
		log.Warn("Got only", replies, "replies for write-propagation")
	}
	return
}

// GetReadRequests returns up to a maximum number of read-requests.
func (s *Service) GetReadRequests(req *ocs.GetReadRequests) (reply *ocs.GetReadRequestsReply, cerr onet.ClientError) {
	reply = &ocs.GetReadRequestsReply{}
	s.saveMutex.Lock()
	current := s.Storage.OCSs.GetByID(req.Start)
	s.saveMutex.Unlock()
	log.Lvlf2("Asking read-requests on writeID: %x", req.Start)

	if current == nil {
		return nil, onet.NewClientErrorCode(ocs.ErrorParameter, "didn't find starting skipblock")
	}
	var doc skipchain.SkipBlockID
	if req.Count == 0 {
		dataOCS := ocs.NewDataOCS(current.Data)
		if dataOCS == nil || dataOCS.Write == nil {
			log.Error("Didn't find this writeID")
			return nil, onet.NewClientErrorCode(ocs.ErrorParameter,
				"id is not a writer-block")
		}
		log.Lvl2("Got first block")
		doc = current.Hash
	}
	for req.Count == 0 || len(reply.Documents) < req.Count {
		if current.Index > 0 {
			// Search next read-request
			dataOCS := ocs.NewDataOCS(current.Data)
			if dataOCS == nil {
				return nil, onet.NewClientErrorCode(ocs.ErrorParameter,
					"unknown block in ocs-skipchain")
			}
			if dataOCS.Read != nil {
				if req.Count == 0 && !dataOCS.Read.DataID.Equal(doc) {
					log.Lvl3("count == 0 and not interesting read found")
					continue
				}
				doc := &ocs.ReadDoc{
					Reader: dataOCS.Read.Public,
					ReadID: current.Hash,
					DataID: dataOCS.Read.DataID,
				}
				log.Lvl2("Found read-request from", doc.Reader)
				reply.Documents = append(reply.Documents, doc)
			}
		}
		if len(current.ForwardLink) > 0 {
			s.saveMutex.Lock()
			current = s.Storage.OCSs.GetFromGenesisByID(current.SkipChainID(),
				current.ForwardLink[0].Hash)
			s.saveMutex.Unlock()
		} else {
			log.Lvl3("No forward-links, stopping")
			break
		}
	}
	log.Lvlf2("WriteID %x: found %d out of a maximum of %d documents", req.Start, len(reply.Documents), req.Count)
	return
}

// GetSharedPublic returns the shared public key of a skipchain.
func (s *Service) SharedPublic(req *ocs.SharedPublicRequest) (reply *ocs.SharedPublicReply, error onet.ClientError) {
	log.Lvl2("Getting shared public key")
	s.saveMutex.Lock()
	shared, ok := s.Storage.Shared[string(req.Genesis)]
	s.saveMutex.Unlock()
	if !ok {
		return nil, onet.NewClientErrorCode(ocs.ErrorParameter, "didn't find this skipchain")
	}
	return &ocs.SharedPublicReply{X: shared.X}, nil
}

// DecryptKeyRequest re-encrypts the stored symmetric key under the public
// key of the read-request. Once the read-request is on the skipchain, it is
// not necessary to check its validity again.
func (s *Service) DecryptKeyRequest(req *ocs.DecryptKeyRequest) (reply *ocs.DecryptKeyReply,
	cerr onet.ClientError) {
	reply = &ocs.DecryptKeyReply{}
	log.Lvl2("Re-encrypt the key to the public key of the reader")

	s.saveMutex.Lock()
	defer s.saveMutex.Unlock()
	readSB := s.Storage.OCSs.GetByID(req.Read)
	read := ocs.NewDataOCS(readSB.Data)
	if read == nil || read.Read == nil {
		return nil, onet.NewClientErrorCode(ocs.ErrorParameter, "This is not a read-block")
	}
	fileSB := s.Storage.OCSs.GetByID(read.Read.DataID)
	file := ocs.NewDataOCS(fileSB.Data)
	if file == nil || file.Write == nil {
		return nil, onet.NewClientErrorCode(ocs.ErrorParameter, "Data-block is broken")
	}

	// Start OCS-protocol to re-encrypt the file's symmetric key under the
	// reader's public key.
	nodes := len(fileSB.Roster.List)
	tree := fileSB.Roster.GenerateNaryTreeWithRoot(nodes, s.ServerIdentity())
	pi, err := s.CreateProtocol(protocol.NameOCS, tree)
	if err != nil {
		return nil, onet.NewClientErrorCode(ocs.ErrorProtocol, err.Error())
	}
	ocsProto := pi.(*protocol.OCS)
	ocsProto.U = file.Write.U
	ocsProto.Xc = read.Read.Public
	log.Printf("Public key is: %s", read.Read.Public)
	ocsProto.Shared = s.Storage.Shared[string(fileSB.GenesisID)]
	ocsProto.SetConfig(&onet.GenericConfig{Data: fileSB.GenesisID})
	err = ocsProto.Start()
	if err != nil {
		return nil, onet.NewClientErrorCode(ocs.ErrorProtocol, err.Error())
	}
	log.Lvl3("Waiting for end of ocs-protocol")
	<-ocsProto.Done
	reply.XhatEnc, err = share.RecoverCommit(network.Suite, ocsProto.Uis,
		nodes-1, nodes)
	if err != nil {
		return nil, onet.NewClientErrorCode(ocs.ErrorProtocol, err.Error())
	}
	reply.Cs = file.Write.Cs
	reply.X = s.Storage.Shared[string(fileSB.GenesisID)].X
	return
}

// GetBunches returns all defined bunches in this conode.
func (s *Service) GetBunches(req *ocs.GetBunchRequest) (reply *ocs.GetBunchReply, cerr onet.ClientError) {
	log.Lvl2("Getting all bunches")
	reply = &ocs.GetBunchReply{}
	s.saveMutex.Lock()
	defer s.saveMutex.Unlock()
	for _, b := range s.Storage.OCSs.Bunches {
		reply.Bunches = append(reply.Bunches, b.GetByID(b.GenesisID))
	}
	return reply, nil
}

// BunchAddBlock adds a block to the latest block from the bunch. If the block
// doesn't have a roster set, it will be copied from the last block.
func (s *Service) BunchAddBlock(bunch *ocs.SkipBlockBunch, r *onet.Roster, data interface{}) (*skipchain.SkipBlock, onet.ClientError) {
	log.Lvl2("Adding block to bunch")
	s.saveMutex.Lock()
	latest := bunch.Latest.Copy()
	s.saveMutex.Unlock()
	reply, err := skipchain.NewClient().StoreSkipBlock(latest, r, data)
	if err != nil {
		return nil, err
	}
	sbNew := reply.Latest
	s.saveMutex.Lock()
	id := bunch.Store(sbNew)
	s.saveMutex.Unlock()
	if id == nil {
		return nil, onet.NewClientErrorCode(ocs.ErrorProtocol,
			"Couldn't add block to bunch")
	}
	return sbNew, nil
}

// NewProtocol intercepts the DKG and OCS protocols to retrieve the values
func (s *Service) NewProtocol(tn *onet.TreeNodeInstance, conf *onet.GenericConfig) (onet.ProtocolInstance, error) {
	//log.Print(s.ServerIdentity(), tn.ProtocolName(), conf)
	switch tn.ProtocolName() {
	case protocol.NameDKG:
		pi, err := protocol.NewSetupDKG(tn)
		if err != nil {
			return nil, err
		}
		setupDKG := pi.(*protocol.SetupDKG)
		go func(conf *onet.GenericConfig) {
			<-setupDKG.Done
			shared, err := setupDKG.SharedSecret()
			if err != nil {
				log.Error(err)
				return
			}
			log.Lvl3(s.ServerIdentity(), "Got shared", shared)
			//log.Print(conf)
			s.saveMutex.Lock()
			s.Storage.Shared[string(conf.Data)] = shared
			s.saveMutex.Unlock()
		}(conf)
		return pi, nil
	case protocol.NameOCS:
		s.saveMutex.Lock()
		shared, ok := s.Storage.Shared[string(conf.Data)]
		s.saveMutex.Unlock()
		if !ok {
			return nil, errors.New("didn't find skipchain")
		}
		pi, err := protocol.NewOCS(tn)
		if err != nil {
			return nil, err
		}
		ocs := pi.(*protocol.OCS)
		ocs.Shared = shared
		return ocs, nil
	}
	return nil, nil
}

// darcRecursive searches for all darcs given an id. It makes sure to avoid
// recursive endless loops by verifying that all new calls are done with
// not-yet-existing IDs.
func (s *Service) darcRecursive(storage map[string]*ocs.Darc, search ocs.DarcID) {
	darc := s.Storage.Accounts[string(search)]
	storage[string(search)] = darc
	log.Printf("%+v", darc)
	for _, d := range darc.Accounts {
		if _, exists := storage[string(d.ID)]; !exists {
			s.darcRecursive(storage, d.ID)
		}
	}
}

func (s *Service) verifyOCS(newID []byte, sb *skipchain.SkipBlock) bool {
	log.Lvl3(s.ServerIdentity(), "Verifying ocs")
	dataOCS := ocs.NewDataOCS(sb.Data)
	if dataOCS == nil {
		log.Lvl3("Didn't find ocs")
		return false
	}
	s.saveMutex.Lock()
	genesis := s.Storage.OCSs.GetFromGenesisByID(sb.SkipChainID(), sb.SkipChainID())
	s.saveMutex.Unlock()
	if genesis == nil {
		log.Lvl3("No genesis-block")
		return false
	}
	ocsData := ocs.NewDataOCS(genesis.Data)
	if ocsData == nil {
		log.Lvl3("No ocs-data in genesis-block")
		return false
	}

	if write := dataOCS.Write; write != nil {
		// Write has to check if the signature comes from a valid writer.
		log.Lvl2("No writing-checking yet")
		return true
	} else if read := dataOCS.Read; read != nil {
		// Read has to check that it's a valid reader
		log.Lvl2("It's a read")
		// Search file
		var writeBlock *ocs.DataOCSWrite
		var readersBlock *ocs.Darc
		for _, sb := range s.Storage.OCSs.GetBunch(genesis.Hash).SkipBlocks {
			wd := ocs.NewDataOCS(sb.Data)
			if wd != nil && wd.Write != nil {
				if bytes.Compare(sb.Hash, read.DataID) == 0 {
					writeBlock = wd.Write
					readersBlock = wd.Readers
					break
				}
			}
		}
		if writeBlock == nil {
			log.Lvl2("Didn't find file")
			return false
		}
		if readersBlock == nil {
			log.Error("Found empty readers-block")
			return false
		}
		for _, pk := range readersBlock.Public {
			err := crypto.VerifySchnorr(network.Suite, pk, read.DataID, *read.Signature)
			if err != nil {
				log.Lvl2("Didn't find signature:", err)
			} else {
				log.Lvl2("Found valid signature from public key", pk)
				return true
			}
		}
		log.Warn("Overriding reader-check!")
		return true
		//return false
	} else if darc := dataOCS.Readers; darc != nil {
		log.Lvl2("Accepting all darc side")
		return true
	}
	return false
}

func (s *Service) propagateOCSFunc(sbI network.Message) {
	sb, ok := sbI.(*skipchain.SkipBlock)
	if !ok {
		log.Error("got something else than a skipblock")
		return
	}
	dataOCS := ocs.NewDataOCS(sb.Data)
	if dataOCS == nil {
		log.Error("Got a skipblock without dataOCS - not storing")
		return
	}
	s.saveMutex.Lock()
	s.Storage.OCSs.Store(sb)
	if r := dataOCS.Readers; r != nil {
		log.Print("Storing new readers", r.ID)
		s.Storage.Accounts[string(r.ID)] = r
	}
	s.saveMutex.Unlock()
	if sb.Index == 0 {
		return
	}
	c := skipchain.NewClient()
	for _, sbID := range sb.BackLinkIDs {
		sbNew, cerr := c.GetSingleBlock(sb.Roster, sbID)
		if cerr != nil {
			log.Error(cerr)
		} else {
			s.saveMutex.Lock()
			s.Storage.OCSs.Store(sbNew)
			s.saveMutex.Unlock()
		}
	}
	s.save()
}

// saves the actual identity
func (s *Service) save() {
	log.Lvl3(s.String(), "Saving service")
	s.saveMutex.Lock()
	defer s.saveMutex.Unlock()
	s.Storage.OCSs.Lock()
	defer s.Storage.OCSs.Unlock()
	for _, b := range s.Storage.OCSs.Bunches {
		b.Lock()
	}
	err := s.Save("storage", s.Storage)
	for _, b := range s.Storage.OCSs.Bunches {
		b.Unlock()
	}
	if err != nil {
		log.Error("Couldn't save file:", err)
	}
}

// Tries to load the configuration and updates if a configuration
// is found, else it returns an error.
func (s *Service) tryLoad() error {
	defer func() {
		if len(s.Storage.Shared) == 0 {
			s.Storage.Shared = map[string]*protocol.SharedSecret{}
		}
		if len(s.Storage.Accounts) == 0 {
			s.Storage.Accounts = map[string]*ocs.Darc{}
		}
	}()
	s.saveMutex.Lock()
	defer s.saveMutex.Unlock()
	if !s.DataAvailable("storage") {
		return nil
	}
	msg, err := s.Load("storage")
	if err != nil {
		return err
	}
	var ok bool
	s.Storage, ok = msg.(*Storage)
	if !ok {
		return errors.New("Data of wrong type")
	}
	log.Lvl3("Successfully loaded")
	return nil
}

// newTemplate receives the context and a path where it can write its
// configuration, if desired. As we don't know when the service will exit,
// we need to save the configuration on our own from time to time.
func newService(c *onet.Context) onet.Service {
	s := &Service{
		ServiceProcessor: onet.NewServiceProcessor(c),
		Storage: &Storage{
			OCSs: ocs.NewSBBStorage(),
		},
	}
	if err := s.RegisterHandlers(s.CreateSkipchains,
		s.WriteRequest, s.ReadRequest, s.GetReadRequests,
		s.DecryptKeyRequest, s.SharedPublic,
		s.GetBunches, s.EditDarc, s.ReadDarc); err != nil {
		log.ErrFatal(err, "Couldn't register messages")
	}
	skipchain.RegisterVerification(c, ocs.VerifyOCS, s.verifyOCS)
	var err error
	s.propagateOCS, err = messaging.NewPropagationFunc(c, "PropagateOCS", s.propagateOCSFunc)
	log.ErrFatal(err)
	if err := s.tryLoad(); err != nil {
		log.Error(err)
	}
	return s
}
