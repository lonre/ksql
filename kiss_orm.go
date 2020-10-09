package kissorm

import (
	"context"
	"fmt"
	"reflect"

	"github.com/jinzhu/gorm"
)

// Client ...
type Client struct {
	tableName string
	db        *gorm.DB
}

// NewClient instantiates a new client
func NewClient(
	dbDriver string,
	connectionString string,
	maxOpenConns int,
	tableName string,
) (Client, error) {
	db, err := gorm.Open(dbDriver, connectionString)
	if err != nil {
		return Client{}, err
	}
	if err = db.DB().Ping(); err != nil {
		return Client{}, err
	}

	db.DB().SetMaxOpenConns(maxOpenConns)

	return Client{
		db:        db,
		tableName: tableName,
	}, nil
}

// ChangeTable creates a new client configured to query on a different table
func (c Client) ChangeTable(ctx context.Context, tableName string) ORMProvider {
	return &Client{
		db:        c.db,
		tableName: tableName,
	}
}

// Find one instance from the database, the input struct
// must be passed by reference and the query should
// return only one result.
func (c Client) Find(
	ctx context.Context,
	item interface{},
	query string,
	params ...interface{},
) error {
	it := c.db.Raw(query, params...)
	if it.Error != nil {
		return it.Error
	}
	it = it.Scan(item)
	return it.Error
}

// QueryChunks is meant to perform queries that returns
// many results and should only be used for that purpose.
//
// It ChunkParser argument will inform the query and its params,
// and the information that will be used to iterate on the results,
// namely:
// (1) The Chunk, which must be a pointer to a slice of structs where
// the results of the query will be kept on each iteration.
// (2) The ChunkSize that describes how many rows should be loaded
// on the Chunk slice before running the iteration callback.
// (3) The ForEachChunk function, which is the iteration callback
// and will be called right after the Chunk is filled with rows
// and/or after the last row is read from the database.
func (c Client) QueryChunks(
	ctx context.Context,
	parser ChunkParser,
) error {
	it := c.db.Raw(parser.Query, parser.Params...)
	if it.Error != nil {
		return it.Error
	}

	rows, err := it.Rows()
	if err != nil {
		return err
	}
	defer rows.Close()

	sliceRef, structType, isSliceOfPtrs, err := decodeAsSliceOfStructs(parser.Chunk)
	if err != nil {
		return err
	}

	slice := sliceRef.Elem()
	var idx = 0
	for ; rows.Next(); idx++ {
		if slice.Len() <= idx {
			var elemValue reflect.Value
			elemValue = reflect.New(structType)
			if !isSliceOfPtrs {
				elemValue = elemValue.Elem()
			}
			slice = reflect.Append(slice, elemValue)
		}

		err = c.db.ScanRows(rows, slice.Index(idx).Addr().Interface())
		if err != nil {
			return err
		}

		if idx == parser.ChunkSize-1 {
			idx = 0
			sliceRef.Elem().Set(slice)
			err = parser.ForEachChunk()
			if err != nil {
				return err
			}
		}
	}

	// If no rows were found or idx was reset to 0
	// on the last iteration skip this last call to ForEachChunk:
	if idx > 0 {
		sliceRef.Elem().Set(slice.Slice(0, idx))
		err = parser.ForEachChunk()
		if err != nil {
			return err
		}
	}

	return nil
}

// Insert one or more instances on the database
//
// If the original instances have been passed by reference
// the ID is automatically updated after insertion is completed.
func (c Client) Insert(
	ctx context.Context,
	items ...interface{},
) error {
	if len(items) == 0 {
		return nil
	}

	for _, item := range items {
		r := c.db.Table(c.tableName).Create(item)
		if r.Error != nil {
			return r.Error
		}
	}

	return nil
}

// Delete deletes one or more instances from the database by id
func (c Client) Delete(
	ctx context.Context,
	ids ...interface{},
) error {
	for _, id := range ids {
		r := c.db.Table(c.tableName).Delete(id)
		if r.Error != nil {
			return r.Error
		}
	}

	return nil
}

// Update updates the given instances on the database by id.
//
// Partial updates are supported, i.e. it will ignore nil pointer attributes
func (c Client) Update(
	ctx context.Context,
	items ...interface{},
) error {
	for _, item := range items {
		m, err := StructToMap(item)
		if err != nil {
			return err
		}
		delete(m, "id")
		r := c.db.Table(c.tableName).Model(item).Updates(m)
		if r.Error != nil {
			return r.Error
		}
	}

	return nil
}

// This cache is kept as a pkg variable
// because the total number of types on a program
// should be finite. So keeping a single cache here
// works fine.
var tagInfoCache = map[reflect.Type]structInfo{}

type structInfo struct {
	Names map[int]string
	Index map[string]int
}

// StructToMap converts any struct type to a map based on
// the tag named `gorm`, i.e. `gorm:"map_key_name"`
//
// This function is efficient in the fact that it caches
// the slower steps of the reflection required to do perform
// this task.
func StructToMap(obj interface{}) (map[string]interface{}, error) {
	v := reflect.ValueOf(obj)
	t := v.Type()

	if t.Kind() == reflect.Ptr {
		t = t.Elem()
		v = v.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("input must be a struct or struct pointer")
	}

	info, found := tagInfoCache[t]
	if !found {
		info = getTagNames(t)
		tagInfoCache[t] = info
	}

	m := map[string]interface{}{}
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		ft := field.Type()
		if ft.Kind() == reflect.Ptr {
			if field.IsNil() {
				continue
			}

			field = field.Elem()
		}

		m[info.Names[i]] = field.Interface()
	}

	return m, nil
}

// This function collects only the names
// that will be used from the input type.
//
// This should save several calls to `Field(i).Tag.Get("foo")`
// which improves performance by a lot.
func getTagNames(t reflect.Type) structInfo {
	info := structInfo{
		Names: map[int]string{},
		Index: map[string]int{},
	}
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Tag.Get("gorm")
		if name == "" {
			continue
		}
		info.Names[i] = name
		info.Index[name] = i
	}

	return info
}

// FillStructWith is meant to be used on unit tests to mock
// the response from the database.
//
// The first argument is any struct you are passing to a kissorm func,
// and the second is a map representing a database row you want
// to use to update this struct.
func FillStructWith(entity interface{}, dbRow map[string]interface{}) error {
	v := reflect.ValueOf(entity)
	t := v.Type()

	if t.Kind() != reflect.Ptr {
		return fmt.Errorf(
			"FillStructWith: expected input to be a pointer to struct but got %T",
			entity,
		)
	}

	t = t.Elem()
	v = v.Elem()

	if t.Kind() != reflect.Struct {
		return fmt.Errorf(
			"FillStructWith: expected input kind to be a struct but got %T",
			entity,
		)
	}

	info, found := tagInfoCache[t]
	if !found {
		info = getTagNames(t)
		tagInfoCache[t] = info
	}

	for colName, attr := range dbRow {
		attrValue := reflect.ValueOf(attr)
		field := v.Field(info.Index[colName])
		fieldType := t.Field(info.Index[colName]).Type

		if !attrValue.Type().ConvertibleTo(fieldType) {
			return fmt.Errorf(
				"FillStructWith: cannot convert atribute %s of type %v to type %T",
				colName,
				fieldType,
				entity,
			)
		}
		field.Set(attrValue.Convert(fieldType))
	}

	return nil
}

// FillSliceWith is meant to be used on unit tests to mock
// the response from the database.
//
// The first argument is any slice of structs you are passing to a kissorm func,
// and the second is a slice of maps representing the database rows you want
// to use to update this struct.
func FillSliceWith(entities interface{}, dbRows []map[string]interface{}) error {
	sliceRef, structType, isSliceOfPtrs, err := decodeAsSliceOfStructs(entities)
	if err != nil {
		return err
	}

	info, found := tagInfoCache[structType]
	if !found {
		info = getTagNames(structType)
		tagInfoCache[structType] = info
	}

	slice := sliceRef.Elem()
	for idx, row := range dbRows {
		if slice.Len() <= idx {
			var elemValue reflect.Value
			elemValue = reflect.New(structType)
			if !isSliceOfPtrs {
				elemValue = elemValue.Elem()
			}
			slice = reflect.Append(slice, elemValue)
		}

		err := FillStructWith(slice.Index(idx).Addr().Interface(), row)
		if err != nil {
			return err
		}
	}

	sliceRef.Elem().Set(slice)

	return nil
}

func decodeAsSliceOfStructs(slice interface{}) (
	sliceRef reflect.Value,
	structType reflect.Type,
	isSliceOfPtrs bool,
	err error,
) {
	slicePtrValue := reflect.ValueOf(slice)
	slicePtrType := slicePtrValue.Type()

	if slicePtrType.Kind() != reflect.Ptr {
		err = fmt.Errorf(
			"FillListWith: expected input to be a pointer to struct but got %T",
			slice,
		)
		return
	}

	t := slicePtrType.Elem()

	if t.Kind() != reflect.Slice {
		err = fmt.Errorf(
			"FillListWith: expected input kind to be a slice but got %T",
			slice,
		)
		return
	}

	elemType := t.Elem()
	isPtr := elemType.Kind() == reflect.Ptr

	if isPtr {
		elemType = elemType.Elem()
	}

	if elemType.Kind() != reflect.Struct {
		err = fmt.Errorf(
			"FillListWith: expected input to be a slice of structs but got %T",
			slice,
		)
		return
	}

	return slicePtrValue, elemType, isPtr, nil
}
