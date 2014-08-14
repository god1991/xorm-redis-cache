package xormrediscache

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"github.com/garyburd/redigo/redis"
	//"github.com/go-xorm/core"
	"hash/crc32"
	"log"
	"reflect"
	// "strconv"
	"time"
)

const (
	DEFAULT_EXPIRATION = time.Duration(0)
	FOREVER_EXPIRATION = time.Duration(-1)
)

// Wraps the Redis client to meet the Cache interface.
type RedisCacher struct {
	pool              *redis.Pool
	defaultExpiration time.Duration
}

// until redigo supports sharding/clustering, only one host will be in hostList
func NewRedisCacher(host string, password string, defaultExpiration time.Duration) *RedisCacher {
	var pool = &redis.Pool{
		MaxIdle:     5,
		IdleTimeout: 240 * time.Second,
		Dial: func() (redis.Conn, error) {
			// the redis protocol should probably be made sett-able
			c, err := redis.Dial("tcp", host)
			if err != nil {
				return nil, err
			}
			if len(password) > 0 {
				if _, err := c.Do("AUTH", password); err != nil {
					c.Close()
					return nil, err
				}
			} else {
				// check with PING
				if _, err := c.Do("PING"); err != nil {
					c.Close()
					return nil, err
				}
			}
			return c, err
		},
		// custom connection test method
		TestOnBorrow: func(c redis.Conn, t time.Time) error {
			if _, err := c.Do("PING"); err != nil {
				return err
			}
			return nil
		},
	}
	return &RedisCacher{pool, defaultExpiration}
}

func exists(conn redis.Conn, key string) bool {
	existed, _ := redis.Bool(conn.Do("EXISTS", key))
	return existed
}

func (c *RedisCacher) getBeanKey(tableName string, id string) string {
	return fmt.Sprintf("bean:%s:%s", tableName, id)
}

func (c *RedisCacher) getSqlKey(tableName string, sql string) string {
	// hash sql to minimize key length
	crc := crc32.ChecksumIEEE([]byte(sql))
	return fmt.Sprintf("sql:%s:%d", tableName, crc)
}

func (c *RedisCacher) Flush() error {
	conn := c.pool.Get()
	defer conn.Close()
	_, err := conn.Do("FLUSHALL")
	return err
}

func (c *RedisCacher) getObject(key string) interface{} {
	conn := c.pool.Get()
	defer conn.Close()
	raw, err := conn.Do("GET", key)
	if raw == nil {
		return nil
	}
	item, err := redis.Bytes(raw, err)
	if err != nil {
		log.Fatalf("[xorm/redis_cacher] redis.Bytes failed: %s", err)
		return nil
	}

	value, err := deserialize(item)

	return value
}

func (c *RedisCacher) getObject2(key string, ptr interface{}) error {
	conn := c.pool.Get()
	defer conn.Close()
	raw, err := conn.Do("GET", key)
	if raw == nil {
		return err
	}
	item, err := redis.Bytes(raw, err)
	if err != nil {
		log.Fatalf("[xorm/redis_cacher] redis.Bytes failed: %s", err)
		return err
	}
	err = deserialize2(item, ptr)
	return err
}

func (c *RedisCacher) GetIds(tableName, sql string) interface{} {
	log.Printf("[xorm/redis_cacher] GetIds|tableName:%s|sql:%s", tableName, sql)

	return c.getObject(c.getSqlKey(tableName, sql))
}

func (c *RedisCacher) GetBean(tableName string, id string) interface{} {
	log.Printf("[xorm/redis_cacher] GetBean|tableName:%s|id:%s", tableName, id)
	return c.getObject(c.getBeanKey(tableName, id))
}

func (c *RedisCacher) GetBean2(tableName string, id string, ptr interface{}) error {
	log.Printf("[xorm/redis_cacher] GetBean|tableName:%s|id:%s", tableName, id)
	return c.getObject2(c.getBeanKey(tableName, id), ptr)
}

func (c *RedisCacher) putObject(key string, value interface{}) {
	c.invoke(c.pool.Get().Do, key, value, c.defaultExpiration)
}

func (c *RedisCacher) PutIds(tableName, sql string, ids interface{}) {
	log.Printf("[xorm/redis_cacher] PutIds|tableName:%s|sql:%s|type:%v", tableName, sql, reflect.TypeOf(ids))

	c.putObject(c.getSqlKey(tableName, sql), ids)
}

func (c *RedisCacher) PutBean(tableName string, id string, obj interface{}) {
	log.Printf("[xorm/redis_cacher] PutBean|tableName:%s|id:%s|type:%v", tableName, id, reflect.TypeOf(obj))
	c.putObject(c.getBeanKey(tableName, id), obj)
}

func (c *RedisCacher) delObject(key string) {
	conn := c.pool.Get()
	defer conn.Close()
	if !exists(conn, key) {
		return // core.ErrCacheMiss
	}
	conn.Do("DEL", key)

	// _, err := conn.Do("DEL", key)
	// return err
}

func (c *RedisCacher) DelIds(tableName, sql string) {
	c.delObject(c.getSqlKey(tableName, sql))
}

func (c *RedisCacher) DelBean(tableName string, id string) {
	c.delObject(c.getBeanKey(tableName, id))
}

func (c *RedisCacher) clearObjects(key string) {
	conn := c.pool.Get()
	defer conn.Close()
	if exists(conn, key) {
		// _, err := conn.Do("DEL", key)
		// return err
		conn.Do("DEL", key)
	} else {
		// return ErrCacheMiss
	}
}

func (c *RedisCacher) ClearIds(tableName string) {
	c.clearObjects(c.getSqlKey(tableName, "*"))
}

func (c *RedisCacher) ClearBeans(tableName string) {
	c.clearObjects(c.getBeanKey(tableName, "*"))
}

func (c *RedisCacher) invoke(f func(string, ...interface{}) (interface{}, error),
	key string, value interface{}, expires time.Duration) error {

	switch expires {
	case DEFAULT_EXPIRATION:
		expires = c.defaultExpiration
	case FOREVER_EXPIRATION:
		expires = time.Duration(0)
	}

	b, err := serialize(value)
	if err != nil {
		return err
	}
	conn := c.pool.Get()
	defer conn.Close()
	if expires > 0 {
		_, err := f("SETEX", key, int32(expires/time.Second), b)
		return err
	} else {
		_, err := f("SET", key, b)
		return err
	}
}

func serialize(value interface{}) ([]byte, error) {

	err := RegisterGobConcreteType(value)
	if err != nil {
		return nil, err
	}

	if reflect.TypeOf(value).Kind() == reflect.Struct {
		return nil, fmt.Errorf("serialize func only take pointer of a struct")
	}

	var b bytes.Buffer
	encoder := gob.NewEncoder(&b)

	log.Printf("[xorm/redis_cacher] interfaceEncode type:%v", reflect.TypeOf(value))
	err = encoder.Encode(&value)
	if err != nil {
		log.Fatalf("[xorm/redis_cacher] gob encoding '%s' failed: %s", value, err)
		return nil, err
	}
	return b.Bytes(), nil
}

func deserialize(byt []byte) (ptr interface{}, err error) {
	b := bytes.NewBuffer(byt)
	decoder := gob.NewDecoder(b)

	var p interface{}
	err = decoder.Decode(&p)
	if err != nil {
		log.Fatal("[xorm/redis_cacher] decode:", err)
		return
	}
	ptr = &p
	log.Printf("[xorm/redis_cacher] deserialize type:%v", reflect.TypeOf(ptr))

	v := reflect.ValueOf(ptr)

	log.Printf("[xorm/redis_cacher] deserialize type:%v | CanAddr:%t", v.Type(), v.CanAddr())

	if v.Kind() == reflect.Struct {
		// TODO need to convert p to pointer of struct, however, encountered reflect.ValueOf(p).CanAddr() == false
		// vv := reflect.New(v.Type())

		// pp := vv.Interface()

		// *pp = p

		// log.Printf("[xorm/redis_cacher] interfaceDecode convert to ptr type:%v|%v", reflect.TypeOf(pp), pp)
	}
	return
}

func deserialize2(byt []byte, ptr interface{}) (err error) {
	b := bytes.NewBuffer(byt)
	decoder := gob.NewDecoder(b)

	log.Printf("[xorm/redis_cacher] deserialize2 type b4 decode:%v", reflect.TypeOf(ptr))

	err = decoder.Decode(ptr)
	if err != nil {
		return
	}
	log.Printf("[xorm/redis_cacher] deserialize2 type:%v", reflect.TypeOf(ptr))

	return
}

func RegisterGobConcreteType(value interface{}) error {

	t := reflect.TypeOf(value)

	log.Printf("[xorm/redis_cacher] RegisterGobConcreteType:%v", t)

	switch t.Kind() {
	case reflect.Ptr:
		v := reflect.ValueOf(value)
		i := v.Elem().Interface()
		gob.Register(i)
	case reflect.Struct, reflect.Map, reflect.Slice:
		gob.Register(value)
	case reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Bool, reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		// do nothing since already registered known type
	default:
		return fmt.Errorf("unhandled type: %v", t)
	}
	return nil
}
