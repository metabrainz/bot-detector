package types

import (
	"bot-detector/internal/utils"
	"strings"
	"testing"
)

func TestGetMatchValue_UnknownField(t *testing.T) {
	entry := &LogEntry{}

	_, _, err := GetMatchValue("UnknownField", entry)

	if err == nil {
		t.Fatal("Expected an error for unknown field, but got nil")
	}

	expectedErrMsg := "unknown field: 'UnknownField'"
	if !strings.Contains(err.Error(), expectedErrMsg) {
		t.Errorf("Error mismatch. Expected error containing '%s', but got: %v", expectedErrMsg, err)
	}
}

func TestGetMatchValue_Success(t *testing.T) {
	entry := &LogEntry{
		IPInfo:     utils.NewIPInfo("192.0.2.1"),
		Path:       "/test/path",
		Method:     "GET",
		Protocol:   "HTTP/1.1",
		UserAgent:  "TestAgent",
		Referrer:   "http://example.com",
		StatusCode: 404,
	}

	testCases := []struct {
		fieldName    string
		expectedVal  interface{}
		expectedType FieldType
	}{
		{"IP", "192.0.2.1", StringField},
		{"Path", "/test/path", StringField},
		{"Method", "GET", StringField},
		{"Protocol", "HTTP/1.1", StringField},
		{"UserAgent", "TestAgent", StringField},
		{"Referrer", "http://example.com", StringField},
		{"StatusCode", 404, IntField},
	}

	for _, tc := range testCases {
		t.Run(tc.fieldName, func(t *testing.T) {
			value, fieldType, err := GetMatchValue(tc.fieldName, entry)
			if err != nil {
				t.Fatalf("Expected no error, but got: %v", err)
			}
			if value != tc.expectedVal {
				t.Errorf("Expected value '%v' (%T), but got '%v' (%T)", tc.expectedVal, tc.expectedVal, value, value)
			}
			if fieldType != tc.expectedType {
				t.Errorf("Expected field type %v, but got %v", tc.expectedType, fieldType)
			}
		})
	}
}

func TestGetMatchValueIfType(t *testing.T) {
	entry := &LogEntry{
		IPInfo:     utils.NewIPInfo("192.0.2.1"),
		Path:       "/test/path",
		StatusCode: 404,
	}

	testCases := []struct {
		name          string
		fieldName     string
		expectedType  FieldType
		expectedValue interface{}
	}{
		{
			name:          "Correct Type - String",
			fieldName:     "Path",
			expectedType:  StringField,
			expectedValue: "/test/path",
		},
		{
			name:          "Correct Type - Int",
			fieldName:     "StatusCode",
			expectedType:  IntField,
			expectedValue: 404,
		},
		{
			name:          "Incorrect Type - Expect String, Got Int",
			fieldName:     "StatusCode",
			expectedType:  StringField,
			expectedValue: nil,
		},
		{
			name:          "Incorrect Type - Expect Int, Got String",
			fieldName:     "Path",
			expectedType:  IntField,
			expectedValue: nil,
		},
		{
			name:          "Unknown Field",
			fieldName:     "UnknownField",
			expectedType:  StringField,
			expectedValue: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			value := GetMatchValueIfType(tc.fieldName, entry, tc.expectedType)

			if value != tc.expectedValue {
				t.Errorf("Expected value '%v' (%T), but got '%v' (%T)", tc.expectedValue, tc.expectedValue, value, value)
			}
		})
	}
}
