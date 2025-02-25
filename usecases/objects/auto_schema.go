//                           _       _
// __      _____  __ ___   ___  __ _| |_ ___
// \ \ /\ / / _ \/ _` \ \ / / |/ _` | __/ _ \
//  \ V  V /  __/ (_| |\ V /| | (_| | ||  __/
//   \_/\_/ \___|\__,_| \_/ |_|\__,_|\__\___|
//
//  Copyright © 2016 - 2023 Weaviate B.V. All rights reserved.
//
//  CONTACT: hello@weaviate.io
//

package objects

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/weaviate/weaviate/entities/additional"
	"github.com/weaviate/weaviate/entities/models"
	"github.com/weaviate/weaviate/entities/schema"
	"github.com/weaviate/weaviate/entities/schema/crossref"
	"github.com/weaviate/weaviate/entities/search"
	"github.com/weaviate/weaviate/usecases/config"
	"github.com/weaviate/weaviate/usecases/objects/validation"
)

type autoSchemaManager struct {
	mutex         sync.RWMutex
	schemaManager schemaManager
	vectorRepo    VectorRepo
	config        config.AutoSchema
	logger        logrus.FieldLogger
}

func newAutoSchemaManager(schemaManager schemaManager, vectorRepo VectorRepo,
	config *config.WeaviateConfig, logger logrus.FieldLogger,
) *autoSchemaManager {
	return &autoSchemaManager{
		schemaManager: schemaManager,
		vectorRepo:    vectorRepo,
		config:        config.Config.AutoSchema,
		logger:        logger,
	}
}

func (m *autoSchemaManager) autoSchema(ctx context.Context, principal *models.Principal,
	object *models.Object, allowCreateClass bool,
) error {
	if m.config.Enabled {
		return m.performAutoSchema(ctx, principal, object, allowCreateClass)
	}
	return nil
}

func (m *autoSchemaManager) performAutoSchema(ctx context.Context, principal *models.Principal,
	object *models.Object, allowCreateClass bool,
) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if object == nil {
		return fmt.Errorf(validation.ErrorMissingObject)
	}

	if len(object.Class) == 0 {
		// stop performing auto schema
		return fmt.Errorf(validation.ErrorMissingClass)
	}

	object.Class = schema.UppercaseClassName(object.Class)

	schemaClass, err := m.getClass(principal, object)
	if err != nil {
		return err
	}
	if schemaClass == nil && !allowCreateClass {
		return fmt.Errorf("given class does not exist")
	}
	properties, err := m.getProperties(object)
	if err != nil {
		return err
	}
	if schemaClass == nil {
		return m.createClass(ctx, principal, object.Class, properties)
	}
	return m.updateClass(ctx, principal, object.Class, properties, schemaClass.Properties)
}

func (m *autoSchemaManager) getClass(principal *models.Principal,
	object *models.Object,
) (*models.Class, error) {
	s, err := m.schemaManager.GetSchema(principal)
	if err != nil {
		return nil, err
	}
	schemaClass := s.GetClass(schema.ClassName(object.Class))
	return schemaClass, nil
}

func (m *autoSchemaManager) createClass(ctx context.Context, principal *models.Principal,
	className string, properties []*models.Property,
) error {
	now := time.Now()
	class := &models.Class{
		Class:       className,
		Properties:  properties,
		Description: "This property was generated by Weaviate's auto-schema feature on " + now.Format(time.ANSIC),
	}
	m.logger.
		WithField("auto_schema", "createClass").
		Debugf("create class %s", className)
	return m.schemaManager.AddClass(ctx, principal, class)
}

func (m *autoSchemaManager) updateClass(ctx context.Context, principal *models.Principal,
	className string, properties []*models.Property, existingProperties []*models.Property,
) error {
	existingPropertiesIndexMap := map[string]int{}
	for index := range existingProperties {
		existingPropertiesIndexMap[existingProperties[index].Name] = index
	}

	propertiesToAdd := []*models.Property{}
	propertiesToUpdate := []*models.Property{}
	for _, prop := range properties {
		index, exists := existingPropertiesIndexMap[schema.LowercaseFirstLetter(prop.Name)]
		if !exists {
			propertiesToAdd = append(propertiesToAdd, prop)
		} else if _, isNested := schema.AsNested(existingProperties[index].DataType); isNested {
			mergedNestedProperties, merged := schema.MergeRecursivelyNestedProperties(existingProperties[index].NestedProperties,
				prop.NestedProperties)
			if merged {
				prop.NestedProperties = mergedNestedProperties
				propertiesToUpdate = append(propertiesToUpdate, prop)
			}
		}
	}
	for _, newProp := range propertiesToAdd {
		m.logger.
			WithField("auto_schema", "updateClass").
			Debugf("update class %s add property %s", className, newProp.Name)
		err := m.schemaManager.AddClassProperty(ctx, principal, className, newProp)
		if err != nil {
			return err
		}
	}
	for _, updatedProp := range propertiesToUpdate {
		m.logger.
			WithField("auto_schema", "updateClass").
			Debugf("update class %s merge object property %s", className, updatedProp.Name)
		err := m.schemaManager.MergeClassObjectProperty(ctx, principal, className, updatedProp)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *autoSchemaManager) getProperties(object *models.Object) ([]*models.Property, error) {
	properties := []*models.Property{}
	if props, ok := object.Properties.(map[string]interface{}); ok {
		for name, value := range props {
			now := time.Now()
			dt, err := m.determineType(value, false)
			if err != nil {
				return nil, fmt.Errorf("property '%s' on class '%s': %w", name, object.Class, err)
			}

			var nestedProperties []*models.NestedProperty
			if len(dt) == 1 {
				switch dt[0] {
				case schema.DataTypeObject:
					nestedProperties, err = m.determineNestedProperties(value.(map[string]interface{}), now)
				case schema.DataTypeObjectArray:
					nestedProperties, err = m.determineNestedPropertiesOfArray(value.([]interface{}), now)
				default:
					// do nothing
				}
			}
			if err != nil {
				return nil, fmt.Errorf("property '%s' on class '%s': %w", name, object.Class, err)
			}

			property := &models.Property{
				Name:             name,
				DataType:         m.getDataTypes(dt),
				Description:      "This property was generated by Weaviate's auto-schema feature on " + now.Format(time.ANSIC),
				NestedProperties: nestedProperties,
			}
			properties = append(properties, property)
		}
	}
	return properties, nil
}

func (m *autoSchemaManager) getDataTypes(dataTypes []schema.DataType) []string {
	dtypes := make([]string, len(dataTypes))
	for i := range dataTypes {
		dtypes[i] = string(dataTypes[i])
	}
	return dtypes
}

func (m *autoSchemaManager) determineType(value interface{}, ofNestedProp bool) ([]schema.DataType, error) {
	fallbackDataType := []schema.DataType{schema.DataTypeText}
	fallbackArrayDataType := []schema.DataType{schema.DataTypeTextArray}

	switch typedValue := value.(type) {
	case string:
		if _, err := time.Parse(time.RFC3339, typedValue); err == nil {
			return []schema.DataType{schema.DataType(m.config.DefaultDate)}, nil
		}
		if _, err := uuid.Parse(typedValue); err == nil {
			return []schema.DataType{schema.DataTypeUUID}, nil
		}
		if m.config.DefaultString != "" {
			return []schema.DataType{schema.DataType(m.config.DefaultString)}, nil
		}
		return []schema.DataType{schema.DataTypeText}, nil
	case json.Number:
		return []schema.DataType{schema.DataType(m.config.DefaultNumber)}, nil
	case float64:
		return []schema.DataType{schema.DataTypeNumber}, nil
	case int64:
		return []schema.DataType{schema.DataTypeInt}, nil
	case bool:
		return []schema.DataType{schema.DataTypeBoolean}, nil
	case map[string]interface{}:
		// nested properties does not support phone and geo data types
		if !ofNestedProp {
			if dt, ok := m.asGeoCoordinatesType(typedValue); ok {
				return dt, nil
			}
			if dt, ok := m.asPhoneNumber(typedValue); ok {
				return dt, nil
			}
		}
		return []schema.DataType{schema.DataTypeObject}, nil
	case []interface{}:
		if len(typedValue) == 0 {
			return fallbackArrayDataType, nil
		}

		refDataTypes := []schema.DataType{}
		var isRef bool
		var determinedDataType schema.DataType

		for i := range typedValue {
			dataType, refDataType, err := m.determineArrayType(typedValue[i], ofNestedProp)
			if err != nil {
				return nil, fmt.Errorf("element [%d]: %w", i, err)
			}
			if i == 0 {
				isRef = refDataType != ""
				determinedDataType = dataType
			}
			if dataType != "" {
				if isRef {
					return nil, fmt.Errorf("element [%d]: mismatched data type - reference expected, got '%s'",
						i, asSingleDataType(dataType))
				}
				if dataType != determinedDataType {
					return nil, fmt.Errorf("element [%d]: mismatched data type - '%s' expected, got '%s'",
						i, asSingleDataType(determinedDataType), asSingleDataType(dataType))
				}
			} else {
				if !isRef {
					return nil, fmt.Errorf("element [%d]: mismatched data type - '%s' expected, got reference",
						i, asSingleDataType(determinedDataType))
				}
				refDataTypes = append(refDataTypes, refDataType)
			}
		}
		if len(refDataTypes) > 0 {
			return refDataTypes, nil
		}
		return []schema.DataType{determinedDataType}, nil
	case nil:
		return fallbackDataType, nil
	default:
		allowed := []string{
			schema.DataTypeText.String(),
			schema.DataTypeNumber.String(),
			schema.DataTypeInt.String(),
			schema.DataTypeBoolean.String(),
			schema.DataTypeDate.String(),
			schema.DataTypeUUID.String(),
			schema.DataTypeObject.String(),
		}
		if !ofNestedProp {
			allowed = append(allowed, schema.DataTypePhoneNumber.String(), schema.DataTypeGeoCoordinates.String())
		}
		return nil, fmt.Errorf("unrecognized data type of value '%v' - one of '%s' expected",
			typedValue, strings.Join(allowed, "', '"))
	}
}

func asSingleDataType(arrayDataType schema.DataType) schema.DataType {
	if dt, isArray := schema.IsArrayType(arrayDataType); isArray {
		return dt
	}
	return arrayDataType
}

func (m *autoSchemaManager) determineArrayType(value interface{}, ofNestedProp bool,
) (schema.DataType, schema.DataType, error) {
	switch typedValue := value.(type) {
	case string:
		if _, err := time.Parse(time.RFC3339, typedValue); err == nil {
			return schema.DataTypeDateArray, "", nil
		}
		if _, err := uuid.Parse(typedValue); err == nil {
			return schema.DataTypeUUIDArray, "", nil
		}
		if schema.DataType(m.config.DefaultString) == schema.DataTypeString {
			return schema.DataTypeStringArray, "", nil
		}
		return schema.DataTypeTextArray, "", nil
	case json.Number:
		if schema.DataType(m.config.DefaultNumber) == schema.DataTypeInt {
			return schema.DataTypeIntArray, "", nil
		}
		return schema.DataTypeNumberArray, "", nil
	case float64:
		return schema.DataTypeNumberArray, "", nil
	case int64:
		return schema.DataTypeIntArray, "", nil
	case bool:
		return schema.DataTypeBooleanArray, "", nil
	case map[string]interface{}:
		if ofNestedProp {
			return schema.DataTypeObjectArray, "", nil
		}
		if refDataType, ok := m.asRef(typedValue); ok {
			return "", refDataType, nil
		}
		return schema.DataTypeObjectArray, "", nil
	default:
		allowed := []string{
			schema.DataTypeText.String(),
			schema.DataTypeNumber.String(),
			schema.DataTypeInt.String(),
			schema.DataTypeBoolean.String(),
			schema.DataTypeDate.String(),
			schema.DataTypeUUID.String(),
			schema.DataTypeObject.String(),
		}
		if !ofNestedProp {
			allowed = append(allowed, schema.DataTypeCRef.String())
		}
		return "", "", fmt.Errorf("unrecognized data type of value '%v' - one of '%s' expected",
			typedValue, strings.Join(allowed, "', '"))
	}
}

func (m *autoSchemaManager) asGeoCoordinatesType(val map[string]interface{}) ([]schema.DataType, bool) {
	if len(val) == 2 {
		if val["latitude"] != nil && val["longitude"] != nil {
			return []schema.DataType{schema.DataTypeGeoCoordinates}, true
		}
	}
	return nil, false
}

func (m *autoSchemaManager) asPhoneNumber(val map[string]interface{}) ([]schema.DataType, bool) {
	if val["input"] != nil {
		if len(val) == 1 {
			return []schema.DataType{schema.DataTypePhoneNumber}, true
		}
		if len(val) == 2 {
			if _, ok := val["defaultCountry"]; ok {
				return []schema.DataType{schema.DataTypePhoneNumber}, true
			}
		}
	}

	return nil, false
}

func (m *autoSchemaManager) asRef(val map[string]interface{}) (schema.DataType, bool) {
	if v, ok := val["beacon"]; ok {
		if beacon, ok := v.(string); ok {
			ref, err := crossref.Parse(beacon)
			if err == nil {
				if ref.Class == "" {
					res, err := m.vectorRepo.ObjectByID(context.Background(), ref.TargetID, search.SelectProperties{}, additional.Properties{}, "")
					if err == nil && res != nil {
						return schema.DataType(res.ClassName), true
					}
				} else {
					return schema.DataType(ref.Class), true
				}
			}
		}
	}
	return "", false
}

func (m *autoSchemaManager) determineNestedProperties(values map[string]interface{}, now time.Time,
) ([]*models.NestedProperty, error) {
	i := 0
	nestedProperties := make([]*models.NestedProperty, len(values))
	for name, value := range values {
		np, err := m.determineNestedProperty(name, value, now)
		if err != nil {
			return nil, fmt.Errorf("nested property '%s': %w", name, err)
		}
		nestedProperties[i] = np
		i++
	}
	return nestedProperties, nil
}

func (m *autoSchemaManager) determineNestedProperty(name string, value interface{}, now time.Time,
) (*models.NestedProperty, error) {
	dt, err := m.determineType(value, true)
	if err != nil {
		return nil, err
	}

	var np []*models.NestedProperty
	if len(dt) == 1 {
		switch dt[0] {
		case schema.DataTypeObject:
			np, err = m.determineNestedProperties(value.(map[string]interface{}), now)
		case schema.DataTypeObjectArray:
			np, err = m.determineNestedPropertiesOfArray(value.([]interface{}), now)
		default:
			// do nothing
		}
	}
	if err != nil {
		return nil, err
	}

	return &models.NestedProperty{
		Name:     name,
		DataType: m.getDataTypes(dt),
		Description: "This nested property was generated by Weaviate's auto-schema feature on " +
			now.Format(time.ANSIC),
		NestedProperties: np,
	}, nil
}

func (m *autoSchemaManager) determineNestedPropertiesOfArray(valArray []interface{}, now time.Time,
) ([]*models.NestedProperty, error) {
	if len(valArray) == 0 {
		return []*models.NestedProperty{}, nil
	}
	nestedProperties, err := m.determineNestedProperties(valArray[0].(map[string]interface{}), now)
	if err != nil {
		return nil, err
	}
	if len(valArray) == 1 {
		return nestedProperties, nil
	}

	nestedPropertiesIndexMap := map[string]int{}
	for index := range nestedProperties {
		nestedPropertiesIndexMap[nestedProperties[index].Name] = index
	}

	for i := 1; i < len(valArray); i++ {
		values := valArray[i].(map[string]interface{})
		for name, value := range values {
			index, ok := nestedPropertiesIndexMap[name]
			if !ok {
				np, err := m.determineNestedProperty(name, value, now)
				if err != nil {
					return nil, err
				}
				nestedPropertiesIndexMap[name] = len(nestedProperties)
				nestedProperties = append(nestedProperties, np)
			} else if _, isNested := schema.AsNested(nestedProperties[index].DataType); isNested {
				np, err := m.determineNestedProperty(name, value, now)
				if err != nil {
					return nil, err
				}
				if mergedNestedProperties, merged := schema.MergeRecursivelyNestedProperties(
					nestedProperties[index].NestedProperties, np.NestedProperties,
				); merged {
					nestedProperties[index].NestedProperties = mergedNestedProperties
				}
			}
		}
	}

	return nestedProperties, nil
}
