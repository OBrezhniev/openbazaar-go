package zend

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/OpenBazaar/openbazaar-go/bitcoin/bitcoind"
	"github.com/OpenBazaar/spvwallet"
	"github.com/OpenBazaar/wallet-interface"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	btcrpcclient "github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	btc "github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/coinset"
	hd "github.com/btcsuite/btcutil/hdkeychain"
	"github.com/btcsuite/btcutil/txsort"
	"github.com/btcsuite/btcwallet/wallet/txauthor"
	"github.com/btcsuite/btcwallet/wallet/txrules"
	"github.com/op/go-logging"
	b39 "github.com/tyler-smith/go-bip39"
)

var log = logging.MustGetLogger("zend")

const (
	Account = "OpenBazaar"
)

type ZendWallet struct {
	params           *chaincfg.Params
	repoPath         string
	trustedPeer      string
	masterPrivateKey *hd.ExtendedKey
	masterPublicKey  *hd.ExtendedKey
	listeners        []func(wallet.TransactionCallback)
	rpcClient        *btcrpcclient.Client
	binary           string
	controlPort      int
	useTor           bool
	addrsToWatch     []btc.Address
	initChan         chan struct{}
}

var connCfg *btcrpcclient.ConnConfig = &btcrpcclient.ConnConfig{
	Host:                 "localhost:8231",
	HTTPPostMode:         true, // zend only supports HTTP POST mode
	DisableTLS:           true, // zend does not provide TLS by default
	DisableAutoReconnect: false,
	DisableConnectOnNew:  false,
}

func NewZendWallet(mnemonic string, params *chaincfg.Params, repoPath string, trustedPeer string, binary string, useTor bool, torControlPort int) (*ZendWallet, error) {
	seed := b39.NewSeed(mnemonic, "")
	mPrivKey, _ := hd.NewMaster(seed, params)
	mPubKey, _ := mPrivKey.Neuter()

	if params.Name == chaincfg.TestNet3Params.Name {
		connCfg.Host = "localhost:18231"
	}

	dataDir := path.Join(repoPath, "zen")

	var err error
	connCfg.User, connCfg.Pass, err = GetCredentials(repoPath)
	if err != nil {
		return nil, err
	}

	if trustedPeer != "" {
		trustedPeer = strings.Split(trustedPeer, ":")[0]
	}

	w := ZendWallet{
		params:           params,
		repoPath:         dataDir,
		trustedPeer:      trustedPeer,
		masterPrivateKey: mPrivKey,
		masterPublicKey:  mPubKey,
		binary:           binary,
		controlPort:      torControlPort,
		useTor:           useTor,
		initChan:         make(chan struct{}),
	}
	return &w, nil
}

func GetCredentials(repoPath string) (username, password string, err error) {
	p := path.Join(repoPath, "zen", "zen.conf")
	if _, err := os.Stat(p); os.IsNotExist(err) {
		dataDir := path.Join(repoPath, "zen")
		os.Mkdir(dataDir, os.ModePerm)

		r := make([]byte, 32)
		_, err := rand.Read(r)
		if err != nil {
			return "", "", err
		}
		password := base64.StdEncoding.EncodeToString(r)

		user := fmt.Sprintf(`rpcuser=%s`, "OpenBazaar")
		pass := fmt.Sprintf(`rpcpassword=%s`, password)

		f, err := os.Create(p)
		if err != nil {
			return "", "", err
		}
		defer f.Close()
		wr := bufio.NewWriter(f)
		fmt.Fprintln(wr, user)
		fmt.Fprintln(wr, pass)
		wr.Flush()
		return "OpenBazaar", password, nil
	} else {
		file, err := os.Open(p)
		if err != nil {
			log.Fatal(err)
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		var unExists, pwExists bool
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "rpcuser=") {
				username = scanner.Text()[8:]
				unExists = true
			} else if strings.Contains(scanner.Text(), "rpcpassword=") {
				password = scanner.Text()[12:]
				pwExists = true
			}
		}
		if !unExists || !pwExists {
			return "", "", errors.New("Zen config file does not contain a username and password")
		}

		if err := scanner.Err(); err != nil {
			return "", "", err
		}
		return username, password, nil
	}
}

func (w *ZendWallet) addQueuedWatchAddresses() {
	for _, addr := range w.addrsToWatch {
		w.addWatchedScript(addr)
	}
}

func (w *ZendWallet) BuildArguments(rescan bool) []string {
	var notify string
	switch runtime.GOOS {
	case "windows":
		notify = `powershell.exe Invoke-WebRequest -Uri http://localhost:8330/ -Method POST -Body %s`
	default:
		notify = `curl -d %s http://localhost:8330/`
	}
	args := []string{"-walletnotify=" + notify, "-server", "-wallet=ob-wallet.dat", "-conf=" + path.Join(w.repoPath, "zen.conf")}
	if rescan {
		args = append(args, "-rescan")
	}
	args = append(args, "-torcontrol=127.0.0.1:"+strconv.Itoa(w.controlPort))

	if w.params.Name == chaincfg.TestNet3Params.Name {
		args = append(args, "-testnet")
	}
	if w.trustedPeer != "" {
		args = append(args, "-connect="+w.trustedPeer)
	}
	if w.useTor {
		socksPort := bitcoind.DefaultSocksPort(w.controlPort)
		args = append(args, "-listen", "-proxy:127.0.0.1:"+strconv.Itoa(socksPort), "-onlynet=onion")
	}
	log.Debug("zend args")
	log.Debug(args)
	return args
}

func (w *ZendWallet) Start() {
	w.shutdownIfActive()
	args := w.BuildArguments(false)
	client, _ := btcrpcclient.New(connCfg, nil)
	w.rpcClient = client
	go bitcoind.StartNotificationListener(client, w.listeners)

	cmd := exec.Command(w.binary, args...)
	go cmd.Start()
	ticker := time.NewTicker(time.Second * 60)
	go func() {
		for range ticker.C {
			log.Fatal("Failed to connect to zend")
		}
	}()
	for {
		_, err := client.GetBlockCount()
		log.Debug(err)
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	ticker.Stop()
	log.Info("Connected to zend")
	close(w.initChan)
	go w.addQueuedWatchAddresses()
}

func (w *ZendWallet) InitChan() chan struct{} {
	return w.initChan
}

// If zend is already running let's shut it down so we restart it with our options
func (w *ZendWallet) shutdownIfActive() {
	client, err := btcrpcclient.New(connCfg, nil)
	if err != nil {
		return
	}
	client.RawRequest("stop", []json.RawMessage{})
	client.Shutdown()
	time.Sleep(5 * time.Second)
}

func (w *ZendWallet) CurrencyCode() string {
	if w.params.Name == chaincfg.MainNetParams.Name {
		return "zen"
	} else {
		return "tzen"
	}
}

func (w *ZendWallet) IsDust(amount int64) bool {
	return txrules.IsDustAmount(btc.Amount(amount), 25, txrules.DefaultRelayFeePerKb)
}

func (w *ZendWallet) MasterPrivateKey() *hd.ExtendedKey {
	return w.masterPrivateKey
}

func (w *ZendWallet) MasterPublicKey() *hd.ExtendedKey {
	return w.masterPublicKey
}

func (w *ZendWallet) CurrentAddress(purpose wallet.KeyPurpose) btc.Address {
	<-w.initChan
	resp, _ := w.rpcClient.RawRequest("getaccountaddress", []json.RawMessage{json.RawMessage([]byte(`""`))})
	var a string
	json.Unmarshal(resp, &a)
	addr, _ := DecodeAddress(a, w.params)
	return addr
}

func (w *ZendWallet) NewAddress(purpose wallet.KeyPurpose) btc.Address {
	<-w.initChan
	resp, _ := w.rpcClient.RawRequest("getnewaddress", []json.RawMessage{json.RawMessage([]byte(`""`))})
	var a string
	json.Unmarshal(resp, &a)
	addr, _ := DecodeAddress(a, w.params)
	return addr
}

func (w *ZendWallet) DecodeAddress(addr string) (btc.Address, error) {
	return DecodeAddress(addr, w.params)
}

func (w *ZendWallet) ScriptToAddress(script []byte) (btc.Address, error) {
	return ExtractPkScriptAddrs(script, w.params)
}

func (w *ZendWallet) AddressToScript(addr btc.Address) ([]byte, error) {

	blockHeight, _ := w.ChainTip()
	blockNumber := int64(blockHeight) - 300
	blockHash, err := w.rpcClient.GetBlockHash(blockNumber)
	if err != nil {
		return nil, err
	}
	return PayToAddrScript(addr, blockHash.CloneBytes(), blockNumber)
}

func (w *ZendWallet) HasKey(addr btc.Address) bool {
	<-w.initChan
	_, err := w.rpcClient.DumpPrivKey(addr)
	if err != nil {
		return false
	}
	return true
}

func (w *ZendWallet) Balance() (confirmed, unconfirmed int64) {
	<-w.initChan
	resp, _ := w.rpcClient.RawRequest("getwalletinfo", []json.RawMessage{})
	type walletInfo struct {
		Balance     float64 `json:"balance"`
		Unconfirmed float64 `json:"unconfirmed_balance"`
	}
	respBytes, _ := resp.MarshalJSON()
	i := new(walletInfo)
	json.Unmarshal(respBytes, i)
	c, _ := btc.NewAmount(i.Balance)
	u, _ := btc.NewAmount(i.Unconfirmed)
	return int64(c.ToUnit(btc.AmountSatoshi)), int64(u.ToUnit(btc.AmountSatoshi))
}

func (w *ZendWallet) GetBlockHeight(hash *chainhash.Hash) (int32, error) {
	<-w.initChan
	h := ``
	if hash != nil {
		h += `"` + hash.String() + `"`
	}
	resp, err := w.rpcClient.RawRequest("getblockheader", []json.RawMessage{json.RawMessage(h)})
	if err != nil {
		return 0, err
	}
	type Respose struct {
		Height int32 `json:"height"`
	}
	r := new(Respose)
	err = json.Unmarshal([]byte(resp), r)
	if err != nil {
		return 0, err
	}

	return r.Height, nil
}

func (w *ZendWallet) FindHeightBeforeTime(ts time.Time) (int32, error) {
	// Get the best block hash
	resp, err := w.rpcClient.RawRequest("getbestblockhash", []json.RawMessage{})
	if err != nil {
		return 0, err
	}
	hash := string(resp)[1 : len(string(resp))-1]

	// Iterate over the block headers to check the timestamp
	for {
		h := `"` + hash + `"`
		resp, err = w.rpcClient.RawRequest("getblockheader", []json.RawMessage{json.RawMessage(h)})
		if err != nil {
			return 0, err
		}
		type Respose struct {
			Timestamp int64  `json:"time"`
			PrevBlock string `json:"previousblockhash"`
			Height    int32  `json:"height"`
		}
		r := new(Respose)
		err = json.Unmarshal([]byte(resp), r)
		if err != nil {
			return 0, err
		}
		t := time.Unix(r.Timestamp, 0)
		if t.Before(ts) || r.Height == 1 {
			return r.Height, nil
		}
		hash = r.PrevBlock
	}
}

func (w *ZendWallet) Transactions() ([]wallet.Txn, error) {
	<-w.initChan
	var ret []wallet.Txn
	resp, err := w.rpcClient.ListTransactions("*")
	if err != nil {
		return ret, err
	}
	for _, r := range resp {
		amt, err := btc.NewAmount(r.Amount)
		if err != nil {
			return ret, err
		}
		ts := time.Unix(r.TimeReceived, 0)
		height := int32(0)
		if r.Confirmations > 0 {
			h, err := chainhash.NewHashFromStr(r.BlockHash)
			if err != nil {
				return ret, err
			}
			height, err = w.GetBlockHeight(h)
			if err != nil {
				return ret, err
			}
		}
		t := wallet.Txn{
			Txid:      r.TxID,
			Value:     int64(amt.ToUnit(btc.AmountSatoshi)),
			Height:    height,
			Timestamp: ts,
		}
		ret = append(ret, t)
	}
	return ret, nil
}

func (w *ZendWallet) GetTransaction(txid chainhash.Hash) (wallet.Txn, error) {
	<-w.initChan
	includeWatchOnly := false
	t := wallet.Txn{}
	resp, err := w.rpcClient.GetTransaction(&txid, &includeWatchOnly)
	if err != nil {
		return t, err
	}
	t.Txid = resp.TxID
	t.Value = int64(resp.Amount * 100000000)
	t.Height = int32(resp.BlockIndex)
	t.Timestamp = time.Unix(resp.TimeReceived, 0)
	t.WatchOnly = false
	return t, nil
}

func (w *ZendWallet) GetConfirmations(txid chainhash.Hash) (uint32, uint32, error) {
	<-w.initChan
	includeWatchOnly := true
	resp, err := w.rpcClient.GetTransaction(&txid, &includeWatchOnly)
	if err != nil {
		return 0, 0, err
	}
	return uint32(resp.Confirmations), uint32(resp.BlockIndex), nil
}

func (w *ZendWallet) ChainTip() (uint32, chainhash.Hash) {
	<-w.initChan
	var ch chainhash.Hash
	info, err := w.rpcClient.GetInfo()
	if err != nil {
		return uint32(0), ch
	}
	h, err := w.rpcClient.GetBestBlockHash()
	if err != nil {
		return uint32(0), ch
	}
	return uint32(info.Blocks), *h
}

func (w *ZendWallet) gatherCoins() (map[coinset.Coin]*hd.ExtendedKey, error) {
	<-w.initChan
	m := make(map[coinset.Coin]*hd.ExtendedKey)
	utxos, err := w.rpcClient.ListUnspent()
	if err != nil {
		return m, err
	}
	for _, u := range utxos {
		if !u.Spendable {
			continue
		}
		txhash, err := chainhash.NewHashFromStr(u.TxID)
		if err != nil {
			return m, err
		}
		addr, err := DecodeAddress(u.Address, w.params)
		if err != nil {
			return m, err
		}
		scriptPubkey, err := w.AddressToScript(addr)
		if err != nil {
			return m, err
		}
		c := spvwallet.NewCoin(txhash.CloneBytes(), u.Vout, btc.Amount(u.Amount*100000000), u.Confirmations, scriptPubkey)

		wif, err := w.rpcClient.DumpPrivKey(addr)
		if err != nil {
			return m, err
		}
		key := hd.NewExtendedKey(
			w.params.HDPrivateKeyID[:],
			wif.PrivKey.Serialize(),
			make([]byte, 32),
			[]byte{0x00, 0x00, 0x00, 0x00},
			0,
			0,
			true)
		m[c] = key
	}
	return m, nil
}

func (w *ZendWallet) Spend(amount int64, addr btc.Address, feeLevel wallet.FeeLevel) (*chainhash.Hash, error) {
	<-w.initChan
	tx, err := w.buildTx(amount, addr, feeLevel)
	if err != nil {
		return nil, err
	}
	return w.rpcClient.SendRawTransaction(tx, false)
}

func (w *ZendWallet) buildTx(amount int64, addr btc.Address, feeLevel wallet.FeeLevel) (*wire.MsgTx, error) {

	blockHeight, _ := w.ChainTip()
	blockNumber := int64(blockHeight) - 300
	blockHash, err := w.rpcClient.GetBlockHash(blockNumber)
	if err != nil {
		return nil, err
	}
	script, _ := PayToAddrScript(addr, blockHash.CloneBytes(), blockNumber)
	if txrules.IsDustAmount(btc.Amount(amount), len(script), txrules.DefaultRelayFeePerKb) {
		return nil, wallet.ErrorDustAmount
	}

	var additionalPrevScripts map[wire.OutPoint][]byte
	var additionalKeysByAddress map[string]*btc.WIF

	// Create input source
	coinMap, err := w.gatherCoins()
	if err != nil {
		return nil, err
	}
	coins := make([]coinset.Coin, 0, len(coinMap))
	for k := range coinMap {
		coins = append(coins, k)
	}
	inputSource := func(target btc.Amount) (total btc.Amount, inputs []*wire.TxIn, scripts [][]byte, err error) {
		coinSelector := coinset.MaxValueAgeCoinSelector{MaxInputs: 10000, MinChangeAmount: btc.Amount(0)}
		coins, err := coinSelector.CoinSelect(target, coins)
		if err != nil {
			return total, inputs, scripts, wallet.ErrorInsuffientFunds
		}
		additionalPrevScripts = make(map[wire.OutPoint][]byte)
		additionalKeysByAddress = make(map[string]*btc.WIF)
		for _, c := range coins.Coins() {
			total += c.Value()
			outpoint := wire.NewOutPoint(c.Hash(), c.Index())
			in := wire.NewTxIn(outpoint, []byte{}, [][]byte{})
			in.Sequence = 0 // Opt-in RBF so we can bump fees
			inputs = append(inputs, in)
			additionalPrevScripts[*outpoint] = c.PkScript()
			key := coinMap[c]
			addr, err := key.Address(w.params)
			if err != nil {
				continue
			}
			privKey, err := key.ECPrivKey()
			if err != nil {
				continue
			}
			wif, _ := btc.NewWIF(privKey, w.params, true)
			additionalKeysByAddress[addr.EncodeAddress()] = wif
		}
		return total, inputs, scripts, nil
	}

	// Get the fee per kilobyte
	feePerKB := int64(w.GetFeePerByte(feeLevel)) * 1000

	// outputs
	out := wire.NewTxOut(amount, script)

	//

	// Create change source
	changeSource := func() ([]byte, error) {
		addr := w.CurrentAddress(wallet.INTERNAL)
		script, err := PayToAddrScript(addr, blockHash.CloneBytes(), blockNumber)
		if err != nil {
			return []byte{}, err
		}
		return script, nil
	}

	outputs := []*wire.TxOut{out}

	authoredTx, err := NewUnsignedTransaction(outputs, btc.Amount(feePerKB), inputSource, changeSource)
	if err != nil {
		return nil, err
	}
	log.Debug("authoredTx txIn", authoredTx.Tx.TxIn)
	// BIP 69 sorting
	txsort.InPlaceSort(authoredTx.Tx)

	//// Sign tx
	//getKey := txscript.KeyClosure(func(addr btc.Address) (*btcec.PrivateKey, bool, error) {
	//	addrStr := addr.EncodeAddress()
	//	wif := additionalKeysByAddress[addrStr]
	//	return wif.PrivKey, wif.CompressPubKey, nil
	//})
	//getScript := txscript.ScriptClosure(func(
	//	addr btc.Address) ([]byte, error) {
	//	return []byte{}, nil
	//})
	//log.Debug("staart signing")

	tx, _, err := w.rpcClient.SignRawTransaction(authoredTx.Tx)
	if err != nil {
		log.Debug(err)
		return nil, errors.New("Failed to sign transaction")
	}

	return tx, nil

	//for i, txIn := range authoredTx.Tx.TxIn {
	//	prevOutScript := additionalPrevScripts[txIn.PreviousOutPoint]
	//	log.Debug("prevOutScript ", prevOutScript)
	//
	//	//try sign tx
	//
	//	script, err := txscript.SignTxOutput(w.params,
	//		authoredTx.Tx, i, prevOutScript, txscript.SigHashAll, getKey,
	//		getScript, txIn.SignatureScript)
	//	if err != nil {
	//		log.Debug("errrrr", err)
	//		return nil, errors.New("Failed to sign transaction")
	//	}
	//	txIn.SignatureScript = script
	//}
	//return authoredTx.Tx, nil
}

func (w *ZendWallet) BumpFee(txid chainhash.Hash) (*chainhash.Hash, error) {
	<-w.initChan
	includeWatchOnly := false
	tx, err := w.rpcClient.GetTransaction(&txid, &includeWatchOnly)
	if err != nil {
		return nil, err
	}
	if tx.Confirmations > 0 {
		return nil, spvwallet.BumpFeeAlreadyConfirmedError
	}
	unspent, err := w.rpcClient.ListUnspent()
	if err != nil {
		return nil, err
	}
	for _, u := range unspent {
		if u.TxID == txid.String() {
			if u.Confirmations > 0 {
				return nil, spvwallet.BumpFeeAlreadyConfirmedError
			}
			h, err := chainhash.NewHashFromStr(u.TxID)
			if err != nil {
				continue
			}
			op := wire.NewOutPoint(h, u.Vout)
			script, err := hex.DecodeString(u.ScriptPubKey)
			if err != nil {
				continue
			}
			utxo := wallet.Utxo{
				Op:           *op,
				Value:        int64(u.Amount / 100000000),
				ScriptPubkey: script,
			}
			addr, err := DecodeAddress(u.Address, w.params)
			if err != nil {
				continue
			}
			key, err := w.rpcClient.DumpPrivKey(addr)
			if err != nil {
				continue
			}
			hdKey := hd.NewExtendedKey(w.Params().HDPrivateKeyID[:], key.PrivKey.Serialize(), make([]byte, 32), make([]byte, 4), 0, 0, true)
			transactionID, err := w.SweepAddress([]wallet.Utxo{utxo}, nil, hdKey, nil, wallet.FEE_BUMP)
			if err != nil {
				return nil, err
			}
			return transactionID, nil

		}
	}
	return nil, spvwallet.BumpFeeNotFoundError
}

func (w *ZendWallet) GetFeePerByte(feeLevel wallet.FeeLevel) uint64 {
	<-w.initChan
	defautlFee := uint64(50)
	var nBlocks json.RawMessage
	switch feeLevel {
	case wallet.PRIOIRTY:
		nBlocks = json.RawMessage([]byte(`1`))
	case wallet.NORMAL:
		nBlocks = json.RawMessage([]byte(`3`))
	case wallet.ECONOMIC:
		nBlocks = json.RawMessage([]byte(`6`))
	default:
		return defautlFee
	}
	resp, err := w.rpcClient.RawRequest("estimatefee", []json.RawMessage{nBlocks})
	if err != nil {
		return defautlFee
	}
	feePerKb, err := strconv.Atoi(string(resp))
	if err != nil {
		return defautlFee
	}
	if feePerKb <= 0 {
		return defautlFee
	}
	fee := feePerKb / 1000
	return uint64(fee)
}

func (w *ZendWallet) EstimateFee(ins []wallet.TransactionInput, outs []wallet.TransactionOutput, feePerByte uint64) uint64 {
	tx := wire.NewMsgTx(wire.TxVersion)
	for _, out := range outs {
		output := wire.NewTxOut(out.Value, out.ScriptPubKey)
		tx.TxOut = append(tx.TxOut, output)
	}
	estimatedSize := spvwallet.EstimateSerializeSize(len(ins), tx.TxOut, false, spvwallet.P2PKH)
	fee := estimatedSize * int(feePerByte)
	return uint64(fee)
}

func (w *ZendWallet) EstimateSpendFee(amount int64, feeLevel wallet.FeeLevel) (uint64, error) {
	<-w.initChan
	addr, err := DecodeAddress("znRLxRK1PwTfWEBruYiuR8r8b24QrQHxXgN", &chaincfg.MainNetParams)
	if err != nil {
		return 0, err
	}
	tx, err := w.buildTx(amount, addr, feeLevel)
	if err != nil {
		return 0, err
	}
	var outval int64
	for _, output := range tx.TxOut {
		outval += output.Value
	}
	var inval int64
	utxos, err := w.rpcClient.ListUnspent()
	if err != nil {
		return 0, err
	}
	for _, input := range tx.TxIn {
		for _, utxo := range utxos {
			if utxo.TxID == input.PreviousOutPoint.Hash.String() && utxo.Vout == input.PreviousOutPoint.Index {
				inval += int64(utxo.Amount * 100000000)
				break
			}
		}
	}
	if inval < outval {
		return 0, errors.New("Error building transaction: inputs less than outputs")
	}
	return uint64(inval - outval), err
}

func (w *ZendWallet) CreateMultisigSignature(ins []wallet.TransactionInput, outs []wallet.TransactionOutput, key *hd.ExtendedKey, redeemScript []byte, feePerByte uint64) ([]wallet.Signature, error) {
	var sigs []wallet.Signature
	tx := wire.NewMsgTx(wire.TxVersion)
	for _, in := range ins {
		ch, err := chainhash.NewHashFromStr(hex.EncodeToString(in.OutpointHash))
		if err != nil {
			return sigs, err
		}
		outpoint := wire.NewOutPoint(ch, in.OutpointIndex)
		input := wire.NewTxIn(outpoint, []byte{}, [][]byte{})
		tx.TxIn = append(tx.TxIn, input)
	}
	for _, out := range outs {
		output := wire.NewTxOut(out.Value, out.ScriptPubKey)
		tx.TxOut = append(tx.TxOut, output)
	}

	// Subtract fee
	estimatedSize := spvwallet.EstimateSerializeSize(len(ins), tx.TxOut, false, spvwallet.P2SH_2of3_Multisig)
	fee := estimatedSize * int(feePerByte)
	feePerOutput := fee / len(tx.TxOut)
	for _, output := range tx.TxOut {
		output.Value -= int64(feePerOutput)
	}

	// BIP 69 sorting
	txsort.InPlaceSort(tx)

	signingKey, err := key.ECPrivKey()
	if err != nil {
		return sigs, err
	}

	// log.Debug("Append to reedem script started" )
	// blockHeight, _ := w.ChainTip()
	// blockNumber := int64(blockHeight) - 300
	// blockHash, err := w.rpcClient.GetBlockHash(blockNumber)

	
	// //append to reedim script
	// builder := txscript.NewScriptBuilder()
	// builder.AddData(redeemScript)
	// builder.AddData(blockHash.CloneBytes()).AddInt64(blockNumber).AddOp(txscript.OP_NOP5)
	// redeemScript, err = builder.Script()

	log.Debug("Append to reedem script stopped")

	for i := range tx.TxIn {
		sig, err := txscript.RawTxInSignature(tx, i, redeemScript, txscript.SigHashAll, signingKey)
		if err != nil {
			continue
		}
		bs := wallet.Signature{InputIndex: uint32(i), Signature: sig}
		sigs = append(sigs, bs)
	}
	return sigs, nil
}

func (w *ZendWallet) Multisign(ins []wallet.TransactionInput, outs []wallet.TransactionOutput, sigs1 []wallet.Signature, sigs2 []wallet.Signature, redeemScript []byte, feePerByte uint64, broadcast bool) ([]byte, error) {
	<-w.initChan
	tx := wire.NewMsgTx(wire.TxVersion)
	for _, in := range ins {
		ch, err := chainhash.NewHashFromStr(hex.EncodeToString(in.OutpointHash))
		if err != nil {
			return nil, err
		}
		outpoint := wire.NewOutPoint(ch, in.OutpointIndex)
		input := wire.NewTxIn(outpoint, []byte{}, [][]byte{})
		tx.TxIn = append(tx.TxIn, input)
	}
	for _, out := range outs {
		output := wire.NewTxOut(out.Value, out.ScriptPubKey)
		tx.TxOut = append(tx.TxOut, output)
	}

	// Subtract fee
	estimatedSize := spvwallet.EstimateSerializeSize(len(ins), tx.TxOut, false, spvwallet.P2SH_2of3_Multisig)
	fee := estimatedSize * int(feePerByte)
	feePerOutput := fee / len(tx.TxOut)
	for _, output := range tx.TxOut {
		output.Value -= int64(feePerOutput)
	}

	// BIP 69 sorting
	txsort.InPlaceSort(tx)

	for i, input := range tx.TxIn {
		var sig1 []byte
		var sig2 []byte
		for _, sig := range sigs1 {
			if int(sig.InputIndex) == i {
				sig1 = sig.Signature
			}
		}
		for _, sig := range sigs2 {
			if int(sig.InputIndex) == i {
				sig2 = sig.Signature
			}
		}
		builder := txscript.NewScriptBuilder()
		builder.AddOp(txscript.OP_0)
		builder.AddData(sig1)
		builder.AddData(sig2)
		builder.AddData(redeemScript)
	
		//blockHeight, _ := w.ChainTip()
		//blockNumber := int64(blockHeight) - 300
		//blockHash, err := w.rpcClient.GetBlockHash(blockNumber)
		//
		//builder.AddData(blockHash.CloneBytes()).AddInt64(blockNumber).AddOp(txscript.OP_NOP5)
		scriptSig, err := builder.Script()

		log.Debug("Create mult sign tx")
		if err != nil {
			return nil, err
		}
		input.SignatureScript = scriptSig
	}
	// Broadcast
	if broadcast {
		_, err := w.rpcClient.SendRawTransaction(tx, false)
		if err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	tx.BtcEncode(&buf, 1, wire.BaseEncoding)
	return buf.Bytes(), nil
}

func (w *ZendWallet) SweepAddress(utxos []wallet.Utxo, address *btc.Address, key *hd.ExtendedKey, redeemScript *[]byte, feeLevel wallet.FeeLevel) (*chainhash.Hash, error) {
	<-w.initChan
	var internalAddr btc.Address
	if address != nil {
		internalAddr = *address
	} else {
		internalAddr = w.CurrentAddress(wallet.INTERNAL)
	}
	blockHeight, _ := w.ChainTip()
	blockNumber := int64(blockHeight) - 300
	blockHash, err := w.rpcClient.GetBlockHash(blockNumber)
	if err != nil {
		return nil, err
	}
	script, err := PayToAddrScript(internalAddr, blockHash.CloneBytes(), blockNumber)
	if err != nil {
		return nil, err
	}

	var val int64
	var inputs []*wire.TxIn
	additionalPrevScripts := make(map[wire.OutPoint][]byte)
	for _, u := range utxos {
		val += u.Value
		in := wire.NewTxIn(&u.Op, []byte{}, [][]byte{})
		inputs = append(inputs, in)
		additionalPrevScripts[u.Op] = u.ScriptPubkey
	}
	out := wire.NewTxOut(val, script)

	txType := spvwallet.P2PKH
	if redeemScript != nil {
		txType = spvwallet.P2SH_1of2_Multisig
	}

	estimatedSize := spvwallet.EstimateSerializeSize(len(utxos), []*wire.TxOut{out}, false, txType)

	// Calculate the fee
	b := json.RawMessage([]byte(`1`))
	resp, err := w.rpcClient.RawRequest("estimatefee", []json.RawMessage{b})
	if err != nil {
		return nil, err
	}
	feePerKb, err := strconv.Atoi(string(resp))
	if err != nil {
		return nil, err
	}
	if feePerKb <= 0 {
		feePerKb = 50000
	}
	fee := estimatedSize * (feePerKb / 1000)

	outVal := val - int64(fee)
	if outVal < 0 {
		outVal = 0
	}
	out.Value = outVal

	tx := &wire.MsgTx{
		Version:  wire.TxVersion,
		TxIn:     inputs,
		TxOut:    []*wire.TxOut{out},
		LockTime: 0,
	}

	// BIP 69 sorting
	txsort.InPlaceSort(tx)

	// Sign tx
	getKey := txscript.KeyClosure(func(addr btc.Address) (*btcec.PrivateKey, bool, error) {
		privKey, err := key.ECPrivKey()
		if err != nil {
			return nil, false, err
		}
		wif, err := btc.NewWIF(privKey, w.params, true)
		if err != nil {
			return nil, false, err
		}
		return wif.PrivKey, wif.CompressPubKey, nil
	})
	getScript := txscript.ScriptClosure(func(addr btc.Address) ([]byte, error) {
		if redeemScript == nil {
			return []byte{}, nil
		}
		return *redeemScript, nil
	})

	for i, txIn := range tx.TxIn {
		prevOutScript := additionalPrevScripts[txIn.PreviousOutPoint]
		script, err := txscript.SignTxOutput(w.params,
			tx, i, prevOutScript, txscript.SigHashAll, getKey,
			getScript, txIn.SignatureScript)
		if err != nil {
			return nil, errors.New("Failed to sign transaction")
		}
		txIn.SignatureScript = script
	}

	// Broadcast
	_, err = w.rpcClient.SendRawTransaction(tx, false)
	if err != nil {
		return nil, err
	}
	txid := tx.TxHash()
	return &txid, nil
}

func (w *ZendWallet) Params() *chaincfg.Params {
	return w.params
}

func (w *ZendWallet) AddTransactionListener(callback func(wallet.TransactionCallback)) {
	w.listeners = append(w.listeners, callback)
}

func (w *ZendWallet) GenerateMultisigScript(keys []hd.ExtendedKey, threshold int, timeout time.Duration, timeoutKey *hd.ExtendedKey) (addr btc.Address, redeemScript []byte, err error) {
	var addrPubKeys []*btc.AddressPubKey
	for _, key := range keys {
		ecKey, err := key.ECPubKey()
		if err != nil {
			return nil, nil, err
		}
		k, err := btc.NewAddressPubKey(ecKey.SerializeCompressed(), w.params)
		if err != nil {
			return nil, nil, err
		}
		addrPubKeys = append(addrPubKeys, k)
	}
	blockHeight, _ := w.ChainTip()
	blockNumber := int64(blockHeight) - 300
	blockHash, err := w.rpcClient.GetBlockHash(blockNumber)
	if err != nil {
		return nil, nil, err
	}

	redeemScript, err = MultiSigScript(addrPubKeys, threshold, blockHash.CloneBytes(), blockNumber)
	if err != nil {
		return nil, nil, err
	}
	addr, err = NewAddressScriptHash(redeemScript, w.params)
	if err != nil {
		return nil, nil, err
	}
	return addr, redeemScript, nil
}

func (w *ZendWallet) AddWatchedScript(script []byte) error {
	addr, err := ExtractPkScriptAddrs(script, w.params)
	if err != nil {
		return err
	}

	select {
	case <-w.initChan:
		return w.addWatchedScript(addr)
	default:
		w.addrsToWatch = append(w.addrsToWatch, addr)
	}
	return nil
}

func (w *ZendWallet) addWatchedScript(addr btc.Address) error {
	a := `"` + addr.EncodeAddress() + `"`
	_, err := w.rpcClient.RawRequest("importaddress", []json.RawMessage{json.RawMessage(a), json.RawMessage(`""`), json.RawMessage(`false`)})
	return err
}

func (w *ZendWallet) ReSyncBlockchain(fromDate time.Time) {
	<-w.initChan
	height, err := w.FindHeightBeforeTime(fromDate)
	if err != nil {
		log.Error(err)
		return
	}
	h := strconv.Itoa(int(height))
	dummyKey := `"SKxuMmhzeBuEYnZo6Hn6NsHpYoD3uniJWYSn6PtNomod1HQ93eoo"`
	_, err = w.rpcClient.RawRequest("z_importkey", []json.RawMessage{json.RawMessage(dummyKey), json.RawMessage(`"yes"`), json.RawMessage(h)})
	if err != nil {
		log.Error(err)
	}
}

func (w *ZendWallet) Close() {
	if w.rpcClient != nil {
		w.rpcClient.RawRequest("stop", []json.RawMessage{})
		w.rpcClient.Shutdown()
	}
}

func NewUnsignedTransaction(outputs []*wire.TxOut, feePerKb btc.Amount, fetchInputs txauthor.InputSource, fetchChange txauthor.ChangeSource) (*txauthor.AuthoredTx, error) {

	var targetAmount btc.Amount
	for _, txOut := range outputs {
		targetAmount += btc.Amount(txOut.Value)
	}

	estimatedSize := EstimateSerializeSize(1, outputs, true, 0)
	targetFee := txrules.FeeForSerializeSize(feePerKb, estimatedSize)

	for {
		inputAmount, inputs, scripts, err := fetchInputs(targetAmount + targetFee)
		if err != nil {
			return nil, err
		}
		if inputAmount < targetAmount+targetFee {
			return nil, errors.New("insufficient funds available to construct transaction")
		}

		maxSignedSize := EstimateSerializeSize(len(inputs), outputs, true, 0)
		maxRequiredFee := txrules.FeeForSerializeSize(feePerKb, maxSignedSize)
		remainingAmount := inputAmount - targetAmount
		if remainingAmount < maxRequiredFee {
			targetFee = maxRequiredFee
			continue
		}

		unsignedTransaction := &wire.MsgTx{
			Version:  wire.TxVersion,
			TxIn:     inputs,
			TxOut:    outputs,
			LockTime: 0,
		}
		changeIndex := -1
		changeAmount := inputAmount - targetAmount - maxRequiredFee
		if changeAmount != 0 && !txrules.IsDustAmount(changeAmount,
			P2PKHOutputSize, txrules.DefaultRelayFeePerKb) {
			changeScript, err := fetchChange()
			if err != nil {
				return nil, err
			}
			log.Debugf("script len", len(changeScript))
			log.Debugf("P2PKHPkScriptSize ", P2PKHPkScriptSize)
			if len(changeScript) > P2PKHPkScriptSize {
				return nil, errors.New("fee estimation requires change " +
					"scripts no larger than P2PKH output scripts")
			}
			change := wire.NewTxOut(int64(changeAmount), changeScript)
			l := len(outputs)
			unsignedTransaction.TxOut = append(outputs[:l:l], change)
			changeIndex = l
		}

		return &txauthor.AuthoredTx{
			Tx:          unsignedTransaction,
			PrevScripts: scripts,
			TotalInput:  inputAmount,
			ChangeIndex: changeIndex,
		}, nil
	}
}
