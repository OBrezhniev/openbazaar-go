package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/OpenBazaar/openbazaar-go/pb"
	"github.com/OpenBazaar/openbazaar-go/repo"
)

// MessagesDB - represents the messages table
type MessagesDB struct {
	modelStore
}

// NewMessageStore - return new MessagesDB
func NewMessageStore(db *sql.DB, lock *sync.Mutex) repo.MessageStore {
	return &MessagesDB{modelStore{db, lock}}
}

// Put - insert record into the messages
func (o *MessagesDB) Put(messageID, orderID string, mType pb.Message_MessageType, peerID string, msg pb.Message) error {
	o.lock.Lock()
	defer o.lock.Unlock()

	tx, err := o.db.Begin()
	if err != nil {
		return err
	}
	stm := `insert or replace into messages(messageID, orderID, message_type, message, peerID, created_at) values(?,?,?,?,?,?)`
	stmt, err := tx.Prepare(stm)
	if err != nil {
		return err
	}

	msg0, err := json.Marshal(msg)
	if err != nil {
		fmt.Println("err marshaling : ", err)
	}

	defer stmt.Close()
	_, err = stmt.Exec(
		messageID,
		orderID,
		int(mType),
		msg0,
		peerID,
		int(time.Now().Unix()),
	)
	if err != nil {
		rErr := tx.Rollback()
		if rErr != nil {
			return fmt.Errorf("message put fail: %s and rollback failed: %s", err.Error(), rErr.Error())
		}
		return err
	}

	return tx.Commit()
}

// GetByOrderIDType returns the message for the specified order and message type
func (o *MessagesDB) GetByOrderIDType(orderID string, mType pb.Message_MessageType) (*pb.Message, string, error) {
	o.lock.Lock()
	defer o.lock.Unlock()
	var (
		msg0   []byte
		peerID string
	)

	stmt, err := o.db.Prepare("select message, peerID from messages where orderID=? and message_type=?")
	if err != nil {
		return nil, "", err
	}
	err = stmt.QueryRow(orderID, mType).Scan(&msg0, &peerID)
	if err != nil {
		return nil, "", err
	}

	msg := new(pb.Message)

	if len(msg0) > 0 {
		err = json.Unmarshal(msg0, msg)
		if err != nil {
			return nil, "", err
		}
	}

	return msg, peerID, nil
}

// GetByMessageIDType returns the message for the specified message id
func (o *MessagesDB) GetByMessageIDType(messageID string) (*pb.Message, string, error) {
	o.lock.Lock()
	defer o.lock.Unlock()
	var (
		msg0   []byte
		peerID string
	)

	stmt, err := o.db.Prepare("select message, peerID from messages where messageID=?")
	if err != nil {
		return nil, "", err
	}
	err = stmt.QueryRow(messageID).Scan(&msg0, &peerID)
	if err != nil {
		return nil, "", err
	}

	msg := new(pb.Message)

	if len(msg0) > 0 {
		err = json.Unmarshal(msg0, msg)
		if err != nil {
			return nil, "", err
		}
	}

	return msg, peerID, nil
}
