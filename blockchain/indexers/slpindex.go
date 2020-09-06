// Copyright (c) 2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package indexers

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	"github.com/gcash/bchd/blockchain"
	"github.com/gcash/bchd/chaincfg/chainhash"
	"github.com/gcash/bchd/database"
	"github.com/gcash/bchd/wire"
	"github.com/gcash/bchutil"
	"github.com/simpleledgerinc/goslp"
	"github.com/simpleledgerinc/goslp/v1parser"
)

const (
	// slpIndexName is the human-readable name for the index.
	slpIndexName = "slp index"
)

var (
	// slpIndexKey is the key of the transaction index and the db bucket used
	// to house it.
	slpIndexKey = []byte("slptxbyhashidx")

	// tokenIDByHashIndexBucketName is the name of the db bucket used to house
	// the token id -> token hash index.
	tokenIDByHashIndexBucketName = []byte("tokenidbyhashidx")

	// tokenMetadataByIDIndexBucketName is the name of the db bucket used to house
	// the token hash -> token id index and token metadata.
	tokenMetadataByIDIndexBucketName = []byte("tokenhashbyididx")

	// errNoTokenIDEntry is an error that indicates a requested entry does
	// not exist in the token ID index.
	errNoTokenIDEntry = errors.New("no entry in the Token ID index")
)

// -----------------------------------------------------------------------------
// The slp index consists of an entry for every SLP-like transaction in the main
// chain.  In order to significantly optimize the space requirements a separate
// index which provides an internal mapping between each TokenID that has been
// indexed and a unique ID for use within the hash to location mappings.  The ID
// is simply a sequentially incremented uint32.  This is useful because it is
// only 4 bytes versus 32 bytes hashes and thus saves a ton of space in the
// index.
//
// There are three buckets used in total.  The first bucket maps the TokenID
// hash to the specific uint32 ID location.  The second bucket maps the
// uint32 of each TokenID to the actual TokenID hash and the third maps that
// unique uint32 ID back to the TokenID hash.
//
//
// The serialized format for keys and values in the TokenID hash to ID bucket is:
//   <hash> = <ID>
//
//   Field           Type              Size
//   TokenID hash    chainhash.Hash    32 bytes
//   ID              uint32            4 bytes
//   -----
//   Total: 36 bytes
//
// The serialized format for keys and values in the ID to TokenID hash bucket is:
//   <ID> = <token id txid><mint baton hash><uint32>
//
//   Field            					Type              Size
//   ID               					uint32            4 bytes
//   TokenID hash                   	chainhash.Hash    32 bytes
//   slp version	    				uint16            2 bytes
//   Mint baton hash (or nft group id)  chainhash.Hash    32 bytes (optional)
//   Mint baton vout  					uint32			  4 bytes  (optional)
//   -----
//   Max: 74 bytes max
//
// The serialized format for the keys and values in the slp index bucket is:
//
//   <txhash> = <token ID><slp version><slp op_return>
//
//   Field           	Type              Size
//   txhash          	chainhash.Hash    32 bytes
//   token ID        	uint32            4 bytes
//   slp version	    uint16            2 bytes
//	 op_return			[]bytes			  typically <220 bytes
//   -----
//   Max: 258 bytes
// -----------------------------------------------------------------------------

// TokenMetadata ...
type TokenMetadata struct {
	TokenID       *chainhash.Hash
	SlpVersion    uint16
	NftGroupID    *chainhash.Hash
	MintBatonHash *chainhash.Hash
	MintBatonVout uint32
}

// dbPutTokenIDIndexEntry uses an existing database transaction to update or add
// the index entries for the hash to id and id to hash mappings for the provided
// values.
func dbPutTokenIDIndexEntry(dbTx database.Tx, id uint32, metadata *TokenMetadata) error {
	// Serialize the height for use in the index entries.
	var serializedID [4]byte
	byteOrder.PutUint32(serializedID[:], id)

	// Add the token ID by token hash mapping to the index.
	meta := dbTx.Metadata()
	hashIndex := meta.Bucket(tokenIDByHashIndexBucketName)
	if err := hashIndex.Put(metadata.TokenID[:], serializedID[:]); err != nil {
		return err
	}

	// Add or update token metadata by uint32 tokenID mapping to the index.
	tmIndex := meta.Bucket(tokenMetadataByIDIndexBucketName)
	tokenMetadata := make([]byte, 32+2+32+4)
	copy(tokenMetadata[0:], metadata.TokenID[:])
	byteOrder.PutUint16(tokenMetadata[32:], metadata.SlpVersion)
	if metadata.NftGroupID != nil {
		copy(tokenMetadata[34:], metadata.NftGroupID[:])
		tokenMetadata = tokenMetadata[:66]
	} else if metadata.MintBatonHash != nil {
		copy(tokenMetadata[34:], metadata.MintBatonHash[:])
		byteOrder.PutUint32(tokenMetadata[66:], metadata.MintBatonVout)
	} else {
		tokenMetadata = tokenMetadata[:34]
	}

	if metadata.NftGroupID == nil && metadata.SlpVersion == 0x41 {
		panic("missing nft group id for NFT child " + string(id))
	}

	return tmIndex.Put(serializedID[:], tokenMetadata)
}

// dbFetchTokenIDByHash uses an existing database transaction to retrieve the
// token id for the provided hash from the index.
func dbFetchTokenIDByHash(dbTx database.Tx, hash *chainhash.Hash) (uint32, error) {
	hashIndex := dbTx.Metadata().Bucket(tokenIDByHashIndexBucketName)
	serializedID := hashIndex.Get(hash[:])
	if serializedID == nil {
		return 0, errNoTokenIDEntry
	}
	return byteOrder.Uint32(serializedID), nil
}

// dbFetchTokenMetadataBySerializedID uses an existing database transaction to
// retrieve the hash for the provided serialized token id from the index.
func dbFetchTokenMetadataBySerializedID(dbTx database.Tx, serializedID []byte) (*TokenMetadata, error) {
	idIndex := dbTx.Metadata().Bucket(tokenMetadataByIDIndexBucketName)
	serializedData := idIndex.Get(serializedID)
	if serializedData == nil {
		return nil, errNoTokenIDEntry
	}

	tokenIDHash, _ := chainhash.NewHash(serializedData[0:32])
	slpVersion := byteOrder.Uint16(serializedData[32:34])

	var (
		mintBatonHash *chainhash.Hash
		mintBatonVout uint32
		nft1GroupID   *chainhash.Hash
	)
	if len(serializedData) == 70 {
		if slpVersion == 0x41 {
			panic("cannon have this length with nft1 child")
		}
		mintBatonHash, _ = chainhash.NewHash(serializedData[34:66])
		mintBatonVout = byteOrder.Uint32(serializedData[66:])
	} else if len(serializedData) == 66 {
		if slpVersion != 0x41 {
			panic("cannot have this length if not nft1 child")
		}
		nft1GroupID, _ = chainhash.NewHash(serializedData[34:])
	}

	tm := &TokenMetadata{
		TokenID:       tokenIDHash,
		NftGroupID:    nft1GroupID,
		MintBatonHash: mintBatonHash,
		MintBatonVout: mintBatonVout,
	}
	return tm, nil
}

// dbFetchTokenMetadataByID uses an existing database transaction to retrieve the
// hash for the provided token id from the index.
func dbFetchTokenMetadataByID(dbTx database.Tx, id uint32) (*TokenMetadata, error) {
	var serializedID [4]byte
	byteOrder.PutUint32(serializedID[:], id)
	return dbFetchTokenMetadataBySerializedID(dbTx, serializedID[:])
}

type dbSlpIndexEntry struct {
	tx             *wire.MsgTx
	slpMsg         *v1parser.ParseResult
	tokenIDHash    *chainhash.Hash
	slpVersion     uint16
	slpMsgPkScript []byte
}

// dbPutSlpIndexEntry uses an existing database transaction to update the
// transaction index given the provided serialized data that is expected to have
// been serialized putSlpIndexEntry.
func dbPutSlpIndexEntry(idx *SlpIndex, dbTx database.Tx, entryInfo *dbSlpIndexEntry) error {
	txHash := entryInfo.tx.TxHash()

	// get current tokenID uint32 for the tokenID hash, add new if needed
	tokenID, err := dbFetchTokenIDByHash(dbTx, entryInfo.tokenIDHash)
	if err != nil {
		tokenID = idx.curTokenID + 1
	}

	var (
		tokenMetadataNeedsUpdated bool   = false
		mintBatonVout             uint32 = 0
		mintBatonHash             *chainhash.Hash
		nft1GroupID               *chainhash.Hash
	)

	if entry, ok := entryInfo.slpMsg.Data.(v1parser.SlpGenesis); ok {
		idx.curTokenID++
		tokenMetadataNeedsUpdated = true
		if entry.MintBatonVout > 1 {
			mintBatonVout = uint32(entry.MintBatonVout)
			mintBatonHash = &txHash
		} else if entryInfo.slpMsg.TokenType == 0x41 {
			parentTokenEntry, _ := dbFetchSlpIndexEntry(dbTx, &entryInfo.tx.TxIn[0].PreviousOutPoint.Hash)
			nft1GroupID = &parentTokenEntry.TokenIDHash
		}
	} else if entry, ok := entryInfo.slpMsg.Data.(v1parser.SlpMint); ok {
		tokenMetadataNeedsUpdated = true
		if entry.MintBatonVout > 1 {
			mintBatonVout = uint32(entry.MintBatonVout)
			mintBatonHash = &txHash
		}
	}

	// maybe update token metadata
	if tokenMetadataNeedsUpdated {
		err = dbPutTokenIDIndexEntry(dbTx, tokenID,
			&TokenMetadata{
				TokenID:       entryInfo.tokenIDHash,
				SlpVersion:    entryInfo.slpVersion,
				MintBatonHash: mintBatonHash,
				MintBatonVout: mintBatonVout,
				NftGroupID:    nft1GroupID,
			})
		if err != nil {
			panic("this should never happen")
		}
	}

	target := make([]byte, 4+2+len(entryInfo.slpMsgPkScript))
	byteOrder.PutUint32(target[:], tokenID)
	byteOrder.PutUint16(target[4:], entryInfo.slpVersion)
	copy(target[6:], entryInfo.slpMsgPkScript)
	slpIndex := dbTx.Metadata().Bucket(slpIndexKey)
	return slpIndex.Put(txHash[:], target)
}

// SlpIndexEntry is a valid SLP token stored in the SLP index
type SlpIndexEntry struct {
	TokenID        uint32
	TokenIDHash    chainhash.Hash
	SlpVersionType uint16
	SlpOpReturn    []byte
}

// dbFetchSlpIndexEntry uses an existing database transaction to fetch the serialized slp
// index entry for the provided transaction hash.  When there is no entry for the provided hash,
// nil will be returned for the both the entry and the error.
func dbFetchSlpIndexEntry(dbTx database.Tx, txHash *chainhash.Hash) (*SlpIndexEntry, error) {
	// Load the record from the database and return now if it doesn't exist.
	SlpIndex := dbTx.Metadata().Bucket(slpIndexKey)
	serializedData := SlpIndex.Get(txHash[:])
	if len(serializedData) == 0 {
		return nil, errors.New("slp entry does not exist " + hex.EncodeToString(txHash[:]))
	}

	// Ensure the serialized data has enough bytes to properly deserialize.
	if len(serializedData) < 12 { // TODO: get more accurate number for this (i.e., 4 + 2 + min SLP length)
		return nil, database.Error{
			ErrorCode: database.ErrCorruption,
			Description: fmt.Sprintf("corrupt slp index "+
				"entry for %s", txHash),
		}
	}
	entry := &SlpIndexEntry{
		TokenID: byteOrder.Uint32(serializedData[0:4]),
	}
	tokenMetadata, err := dbFetchTokenMetadataByID(dbTx, entry.TokenID)
	if err != nil {
		return nil, err
	}
	entry.TokenIDHash = *tokenMetadata.TokenID
	entry.SlpVersionType = byteOrder.Uint16(serializedData[4:6])
	entry.SlpOpReturn = serializedData[6:]
	return entry, nil
}

// dbRemoveSlpIndexEntries uses an existing database transaction to remove the
// latest slp transaction entry for every transaction in the passed block.
//
// This method should only be called by DisconnectBlock()
//
func dbRemoveSlpIndexEntries(dbTx database.Tx, block *bchutil.Block) error {
	// toposort and reverse order so we can unwind slp token metadata state if needed
	txs := TopoSortTxs(block.Transactions())
	var txsRev []*wire.MsgTx
	for i := len(txs) - 1; i >= 0; i-- {
		txsRev = append(txsRev, txs[i])
	}

	// this method should only be called after a topo sort
	dbRemoveSlpIndexEntry := func(dbTx database.Tx, txHash *chainhash.Hash) error {
		slpIndex := dbTx.Metadata().Bucket(slpIndexKey)
		serializedData := slpIndex.Get(txHash[:])
		if len(serializedData) == 0 {
			return nil
		}

		// NOTE: In the future token metadata may contain properties which
		// need to be updated here.  Currently, token metadata only have mint baton location
		// and NFT1 group ID.  Neither of these items need special handling
		// on DisconnectBlock since they are properly updated during
		// the subsequently called ConnectBlock.

		return slpIndex.Delete(txHash[:])
	}

	for _, tx := range txsRev {
		hash := tx.TxHash()
		err := dbRemoveSlpIndexEntry(dbTx, &hash)
		if err != nil {
			return err
		}
	}

	return nil
}

// SlpIndex implements a transaction by hash index.  That is to say, it supports
// querying all transactions by their hash.
type SlpIndex struct {
	db         database.DB
	curTokenID uint32
	config     *SlpConfig
	cache      *SlpCache
}

// Ensure the SlpIndex type implements the Indexer interface.
var _ Indexer = (*SlpIndex)(nil)

// Init initializes the hash-based slp transaction index.  In particular, it finds
// the highest used Token ID and stores it for later use when a new token has been
// created.
//
// This is part of the Indexer interface.
func (idx *SlpIndex) Init() error {
	// Find the latest known token id field for the internal token id
	// index and initialize it.  This is done because it's a lot more
	// efficient to do a single search at initialize time than it is to
	// write another value to the database on every update.
	err := idx.db.View(func(dbTx database.Tx) error {
		var highestKnown, nextUnknown uint32
		testTokenID := uint32(1)
		increment := uint32(1)
		for {
			_, err := dbFetchTokenMetadataByID(dbTx, testTokenID)
			if err != nil {
				nextUnknown = testTokenID
				break
			}

			highestKnown = testTokenID
			testTokenID += increment
		}
		log.Tracef("Forward scan (highest known %d, next unknown %d)",
			highestKnown, nextUnknown)

		idx.curTokenID = highestKnown
		return nil
	})

	if err != nil {
		return err
	}

	log.Info("Current number of SLP tokens in index: " + fmt.Sprint(idx.curTokenID))
	return nil
}

// StartBlock is used to indicate the proper start block for the index manager.
//
// This is part of the Indexer interface.
func (idx *SlpIndex) StartBlock() (*chainhash.Hash, int32) {
	return idx.config.StartHash, idx.config.StartHeight
}

// Migrate is only provided to satisfy the Indexer interface as there is nothing to
// migrate this index.
//
// This is part of the Indexer interface.
func (idx *SlpIndex) Migrate(db database.DB, interrupt <-chan struct{}) error {
	// Nothing to do.
	return nil
}

// Key returns the database key to use for the index as a byte slice.
//
// This is part of the Indexer interface.
func (idx *SlpIndex) Key() []byte {
	return slpIndexKey
}

// Name returns the human-readable name of the index.
//
// This is part of the Indexer interface.
func (idx *SlpIndex) Name() string {
	return slpIndexName
}

// Create is invoked when the indexer manager determines the index needs
// to be created for the first time.  It creates the buckets for the hash-based
// transaction index and the internal token ID and token metadata indexes.
//
// This is part of the Indexer interface.
func (idx *SlpIndex) Create(dbTx database.Tx) error {
	meta := dbTx.Metadata()
	if _, err := meta.CreateBucket(tokenIDByHashIndexBucketName); err != nil {
		return err
	}
	if _, err := meta.CreateBucket(tokenMetadataByIDIndexBucketName); err != nil {
		return err
	}
	_, err := meta.CreateBucket(slpIndexKey)
	return err
}

// BurnedInput represents a burned slp txo item
type BurnedInput struct {
	Tx      *wire.MsgTx
	TxInput *wire.TxIn
	SlpMsg  *v1parser.ParseResult
	Entry   *SlpIndexEntry
}

// ConnectBlock is invoked by the index manager when a new block has been
// connected to the main chain.  This indexer adds a hash-to-transaction mapping
// for every transaction in the passed block.
//
// This is part of the Indexer interface.
func (idx *SlpIndex) ConnectBlock(dbTx database.Tx, block *bchutil.Block, stxos []blockchain.SpentTxOut) error {

	sortedTxns := TopoSortTxs(block.Transactions())

	getSlpIndexEntry := func(txiHash *chainhash.Hash) (*SlpIndexEntry, error) {
		return idx.GetSlpIndexEntry(dbTx, txiHash)
	}

	putTxIndexEntry := func(tx *wire.MsgTx, slpMsg *v1parser.ParseResult, tokenIDHash *chainhash.Hash) error {
		entry := &dbSlpIndexEntry{
			tx:             tx,
			slpMsg:         slpMsg,
			tokenIDHash:    tokenIDHash,
			slpMsgPkScript: tx.TxOut[0].PkScript,
			slpVersion:     uint16(slpMsg.TokenType),
		}
		return dbPutSlpIndexEntry(idx, dbTx, entry)
	}

	burnedInputs := make([]*BurnedInput, 0)
	for _, tx := range sortedTxns {
		isValid, txnInputsBurned := CheckSlpTx(tx, getSlpIndexEntry, putTxIndexEntry)

		// look for burned inputs within non-SLP txns
		if !isValid {
			for _, txi := range tx.TxIn {
				slpEntry, _ := idx.GetSlpIndexEntry(dbTx, &txi.PreviousOutPoint.Hash)
				if slpEntry != nil {
					slpMsg, _ := v1parser.ParseSLP(slpEntry.SlpOpReturn)
					burnedInputs = append(burnedInputs, &BurnedInput{
						Tx:      tx,
						TxInput: txi,
						SlpMsg:  slpMsg,
						Entry:   slpEntry,
					})
				}
			}
		}

		if txnInputsBurned != nil {
			burnedInputs = append(burnedInputs, txnInputsBurned...)
		}
	}

	// Loop through burned inputs and check for different situations
	// where token metadata will need to be updated.
	//
	// NOTE: items in burnedInputs are not topologically ordered
	//
	for _, burn := range burnedInputs {
		// Currently we only need to check for a burned mint baton
		isMintBatonBurned := idx.checkBurnedInputForMintBaton(dbTx, burn)
		if isMintBatonBurned {
			continue
		}
	}
	return nil
}

func (idx *SlpIndex) checkBurnedInputForMintBaton(dbTx database.Tx, burn *BurnedInput) bool {

	// we can skip nft children since they don't have mint batons
	if burn.SlpMsg.TokenType == 0x41 {
		return false
	}

	// check if input is the mint baton from either Genesis or Mint parent data
	if msg, ok := burn.SlpMsg.Data.(v1parser.SlpGenesis); ok {
		if msg.MintBatonVout != int(burn.TxInput.PreviousOutPoint.Index) {
			return false
		}
	} else if msg, ok := burn.SlpMsg.Data.(v1parser.SlpMint); ok {
		if msg.MintBatonVout != int(burn.TxInput.PreviousOutPoint.Index) {
			return false
		}
	} else {
		return false
	}

	// double-check this burned mint baton was a valid slp token
	if burn.Entry == nil {
		return false
	}

	err := dbPutTokenIDIndexEntry(dbTx, burn.Entry.TokenID,
		&TokenMetadata{
			TokenID:       &burn.Entry.TokenIDHash,
			SlpVersion:    burn.Entry.SlpVersionType,
			MintBatonHash: nil,
			MintBatonVout: 0,
			NftGroupID:    nil,
		},
	)
	if err != nil {
		panic("could not update token metadata")
	}

	return true
}

// GetSlpIndexEntryHandler ...
type GetSlpIndexEntryHandler func(*chainhash.Hash) (*SlpIndexEntry, error)

// AddTxIndexEntryHandler ...
type AddTxIndexEntryHandler func(*wire.MsgTx, *v1parser.ParseResult, *chainhash.Hash) error

// CheckSlpTx checks a transaction for validity and adds valid txns to the db
func CheckSlpTx(tx *wire.MsgTx, getSlpIndexEntry GetSlpIndexEntryHandler, putTxIndexEntry AddTxIndexEntryHandler) (bool, []*BurnedInput) {

	txSlpMsg, _ := v1parser.ParseSLP(tx.TxOut[0].PkScript)
	if txSlpMsg == nil {
		return false, nil
	}

	burnedInputs := make([]*BurnedInput, 0)

	tokenID, err := goslp.GetSlpTokenID(tx)
	if err != nil {
		panic(err.Error())
	}
	tokenIDHash, _ := chainhash.NewHash(tokenID[:])

	v1InputAmtSpent := big.NewInt(0)
	v1MintBatonVout := 0

	// loop through inputs to look for valid slp contributions, and check for burned inputs
	for i, txi := range tx.TxIn {
		prevIdx := int(txi.PreviousOutPoint.Index)

		slpEntry, err := getSlpIndexEntry(&txi.PreviousOutPoint.Hash)
		if slpEntry == nil {
			continue
		}

		inputSlpMsg, err := v1parser.ParseSLP(slpEntry.SlpOpReturn)
		if err != nil {
			panic("previously saved slp scriptPubKey cannot be parsed.")
		}

		amt, _ := inputSlpMsg.GetVoutAmount(prevIdx)
		if txSlpMsg.TokenType == 0x41 && txSlpMsg.TransactionType == "GENESIS" { // checks inputs for NFT1 child GENESIS
			if inputSlpMsg.TokenType == 0x81 && i == 0 {
				v1InputAmtSpent.Add(v1InputAmtSpent, amt)
			}
		} else if slpEntry.TokenIDHash.Compare(tokenIDHash) == 0 && inputSlpMsg.TokenType != txSlpMsg.TokenType { // checks SEND/MINT inputs
			if txSlpMsg.TransactionType == "MINT" {
				if msg, ok := inputSlpMsg.Data.(v1parser.SlpGenesis); ok {
					if prevIdx == msg.MintBatonVout {
						v1MintBatonVout = prevIdx
					}
				} else if msg, ok := inputSlpMsg.Data.(v1parser.SlpMint); ok {
					if prevIdx == msg.MintBatonVout {
						v1MintBatonVout = prevIdx
					}
				}
			} else { // i.e., SEND
				v1InputAmtSpent.Add(v1InputAmtSpent, amt)

				// catch mint batons burned in a valid SEND transaction
				if msg, ok := inputSlpMsg.Data.(v1parser.SlpGenesis); ok {
					if prevIdx == msg.MintBatonVout {
						burnedInputs = append(burnedInputs, &BurnedInput{
							Tx:      tx,
							TxInput: txi,
							SlpMsg:  inputSlpMsg,
							Entry:   slpEntry,
						})
					}
				} else if msg, ok := inputSlpMsg.Data.(v1parser.SlpMint); ok {
					if prevIdx == msg.MintBatonVout {
						burnedInputs = append(burnedInputs, &BurnedInput{
							Tx:      tx,
							TxInput: txi,
							SlpMsg:  inputSlpMsg,
							Entry:   slpEntry,
						})
					}
				}
			}

			if inputSlpMsg.TransactionType == "GENESIS" { // check for minting baton
				if prevIdx == inputSlpMsg.Data.(v1parser.SlpGenesis).MintBatonVout {
					v1MintBatonVout = prevIdx
				}
			} else if inputSlpMsg.TransactionType == "MINT" {
				if prevIdx == inputSlpMsg.Data.(v1parser.SlpMint).MintBatonVout {
					v1MintBatonVout = prevIdx
				}
			}
		} else {
			burnedInputs = append(burnedInputs, &BurnedInput{
				Tx:      tx,
				TxInput: txi,
				SlpMsg:  inputSlpMsg,
				Entry:   slpEntry,
			})
		}
	}

	// TODO: check/handle edge case where GENESIS or MINT transaction has new mint baton
	// at a non-existant output index.  The Token Metadata needs to be update to show the
	// mint baton as burned.

	// Check if tx is a valid SLP. SLP validity has two requirements:
	//  (1) the slpMsg must be valid, and
	//  (2) the input requirements must be satisfied.
	isValid := false
	outputAmt, _ := txSlpMsg.TotalSlpMsgOutputValue()
	if txSlpMsg.TransactionType == "GENESIS" {
		if txSlpMsg.TokenType == 0x41 &&
			big.NewInt(1).Cmp(v1InputAmtSpent) < 1 {
			isValid = true
		} else if txSlpMsg.TokenType == 0x01 || txSlpMsg.TokenType == 0x81 {
			isValid = true
		}
	} else if txSlpMsg.TransactionType == "SEND" &&
		outputAmt.Cmp(v1InputAmtSpent) < 1 {
		isValid = true
	} else if txSlpMsg.TransactionType == "MINT" &&
		v1MintBatonVout > 1 {
		isValid = true
	}

	if isValid {
		err := putTxIndexEntry(tx, txSlpMsg, tokenIDHash)
		if err != nil {
			panic(err.Error())
		}
	}
	return isValid, burnedInputs
}

// DisconnectBlock is invoked by the index manager when a block has been
// disconnected from the main chain.  This indexer removes the
// hash-to-transaction mapping for every transaction in the block.
//
// This is part of the Indexer interface.
func (idx *SlpIndex) DisconnectBlock(dbTx database.Tx, block *bchutil.Block, stxos []blockchain.SpentTxOut) error {

	// Remove all of the transactions in the block from the index.
	if err := dbRemoveSlpIndexEntries(dbTx, block); err != nil {
		return err
	}

	return nil
}

// GetSlpIndexEntry returns a serialized slp index entry for the provided transaction hash
// from the slp index.  The slp index entry can in turn be used to quickly discover
// additional slp information about the transaction. When there is no entry for the provided hash, nil
// will be returned for the both the entry and the error, which would mean the transaction is invalid
//
// This function is safe for concurrent access.
func (idx *SlpIndex) GetSlpIndexEntry(dbTx database.Tx, hash *chainhash.Hash) (*SlpIndexEntry, error) {
	entry := idx.cache.Get(hash)
	if entry != nil {
		return entry, nil
	}

	entry, err := dbFetchSlpIndexEntry(dbTx, hash)
	if err != nil {
		return nil, err
	}

	idx.cache.AddTemp(hash, entry)
	return entry, nil
}

// GetTokenMetadata ...
func (idx *SlpIndex) GetTokenMetadata(dbTx database.Tx, tokenID uint32) (*TokenMetadata, error) {
	serializedID := make([]byte, 4)
	byteOrder.PutUint32(serializedID, tokenID)
	return dbFetchTokenMetadataBySerializedID(dbTx, serializedID)
}

// AddMempoolTx adds a new SlpIndexEntry item to a temporary cache that holds
// both mempool and recently queried db entries.
//
// TODO: How do we handle fetching of TokenMetadata for Genesis txns in the mempool?
//
func (idx *SlpIndex) AddMempoolTx(tx *bchutil.Tx) error {

	slpMsg, err := v1parser.ParseSLP(tx.MsgTx().TxOut[0].PkScript)
	if err != nil {
		return err
	}

	if slpMsg.TokenType != 0x01 && slpMsg.TokenType != 0x41 && slpMsg.TokenType != 0x81 {
		return errors.New("unsupported token type")
	}

	_tokenIDHash, _ := goslp.GetSlpTokenID(tx.MsgTx())
	tokenIDHash, _ := chainhash.NewHash(_tokenIDHash)
	idx.cache.AddMempoolItem(tx.Hash(), &SlpIndexEntry{
		TokenID:        0,
		TokenIDHash:    *tokenIDHash,
		SlpVersionType: uint16(slpMsg.TokenType),
		SlpOpReturn:    tx.MsgTx().TxOut[0].PkScript,
	})
	return nil
}

// RemoveMempoolTxs removes a list of transactions from the temporary cache that holds
// both mempool and recently queried SlpIndexEntries
func (idx *SlpIndex) RemoveMempoolTxs(txs []*bchutil.Tx) {
	idx.cache.RemoveMempoolItems(txs)
}

// SlpIndexEntryExists returns true if the slp entry exists
func (idx *SlpIndex) SlpIndexEntryExists(dbTx database.Tx, txHash *chainhash.Hash) bool {
	slpIndex := dbTx.Metadata().Bucket(slpIndexKey)
	serializedData := slpIndex.Get(txHash[:])
	return len(serializedData) != 0
}

// SlpConfig provides the proper starting height and hash
type SlpConfig struct {
	StartHash    *chainhash.Hash
	StartHeight  int32
	AddrPrefix   string
	MaxCacheSize int
}

// NewSlpIndex returns a new instance of an indexer that is used to create a
// mapping of the hashes of all slp transactions in the blockchain to the respective
// token ID, and token metadata.
//
// It implements the Indexer interface which plugs into the IndexManager that in
// turn is used by the blockchain package.  This allows the index to be
// seamlessly maintained along with the chain.
func NewSlpIndex(db database.DB, cfg *SlpConfig) *SlpIndex {
	return &SlpIndex{
		db:     db,
		config: cfg,
		cache:  NewSlpCache(cfg.MaxCacheSize),
	}
}

// dropTokenIndexes drops the internal token id index.
func dropTokenIndexes(db database.DB) error {
	return db.Update(func(dbTx database.Tx) error {
		meta := dbTx.Metadata()
		err := meta.DeleteBucket(tokenIDByHashIndexBucketName)
		if err != nil {
			return err
		}

		return meta.DeleteBucket(tokenMetadataByIDIndexBucketName)
	})
}

// DropSlpIndex drops the transaction index from the provided database if it
// exists.  Since the address index relies on it, the address index will also be
// dropped when it exists.
func DropSlpIndex(db database.DB, interrupt <-chan struct{}) error {
	err := dropIndex(db, slpIndexKey, slpIndexName, interrupt)
	if err != nil {
		return err
	}

	return dropIndex(db, slpIndexKey, slpIndexName, interrupt)
}

// TopoSortTxs sorts a list of transactions into topological order.
// That is, the child transactions come after parents.
func TopoSortTxs(transactions []*bchutil.Tx) []*wire.MsgTx {

	sorted := make([]*wire.MsgTx, 0, len(transactions))
	txids := make(map[chainhash.Hash]struct{})
	outpoints := make(map[wire.OutPoint]struct{})

	for _, tx := range transactions {
		for i := range tx.MsgTx().TxOut {
			op := wire.OutPoint{
				Hash:  *tx.Hash(),
				Index: uint32(i),
			}
			outpoints[op] = struct{}{}
		}
	}

	for len(sorted) < len(transactions) {
		for _, tx := range transactions {
			if _, ok := txids[*tx.Hash()]; ok {
				continue
			}
			foundParent := false
			for _, in := range tx.MsgTx().TxIn {
				if _, ok := outpoints[in.PreviousOutPoint]; ok {
					foundParent = true
					break
				}
			}
			if !foundParent {
				sorted = append(sorted, tx.MsgTx())
				for i := range tx.MsgTx().TxOut {
					op := wire.OutPoint{
						Hash:  *tx.Hash(),
						Index: uint32(i),
					}
					delete(outpoints, op)
				}
				txids[*tx.Hash()] = struct{}{}
			}
		}
	}
	return sorted
}

func removeDups(txs []*wire.MsgTx) []*wire.MsgTx {
	keys := make(map[*wire.MsgTx]bool)
	var ret []*wire.MsgTx
	for _, tx := range txs {
		if _, ok := keys[tx]; !ok {
			keys[tx] = true
			ret = append(ret, tx)
		}
	}
	return ret
}
