package sqlite

import modernc "modernc.org/sqlite"

type statementPolicy uint8

const (
	transactionStatements statementPolicy = iota
	readStatements
)

type pragmaName string

const (
	pragmaDatabaseList      pragmaName = "database_list"
	pragmaTableList         pragmaName = "table_list"
	pragmaTableInfo         pragmaName = "table_info"
	pragmaTableExtendedInfo pragmaName = "table_xinfo"
	pragmaIndexList         pragmaName = "index_list"
	pragmaIndexInfo         pragmaName = "index_info"
	pragmaIndexExtendedInfo pragmaName = "index_xinfo"
	pragmaForeignKeyList    pragmaName = "foreign_key_list"
	pragmaCollationList     pragmaName = "collation_list"
	pragmaCompileOptions    pragmaName = "compile_options"
	pragmaFunctionList      pragmaName = "function_list"
	pragmaModuleList        pragmaName = "module_list"
	pragmaPragmaList        pragmaName = "pragma_list"
)

// Connection settings and transaction boundaries belong to the connection owner.
// Even an empty PRAGMA argument can change a setting, so configurable PRAGMAs
// cannot be admitted by testing whether the authorizer's argument is empty.
func (p statementPolicy) authorize(action modernc.AuthorizerActionCode, first, _, _, _ string) modernc.AuthorizerReturnCode {
	switch action {
	case modernc.AuthTransaction, modernc.AuthSavepoint, modernc.AuthAttach, modernc.AuthDetach:
		return modernc.AuthorizerDeny
	case modernc.AuthPragma:
		if pragmaName(first).inspectionOnly() {
			return modernc.AuthorizerOK
		}
		return modernc.AuthorizerDeny
	case modernc.AuthRead, modernc.AuthSelect, modernc.AuthFunction, modernc.AuthRecursive:
		return modernc.AuthorizerOK
	default:
		if p == readStatements {
			return modernc.AuthorizerDeny
		}
		return modernc.AuthorizerOK
	}
}

func (name pragmaName) inspectionOnly() bool {
	switch name {
	case pragmaDatabaseList, pragmaTableList, pragmaTableInfo, pragmaTableExtendedInfo,
		pragmaIndexList, pragmaIndexInfo, pragmaIndexExtendedInfo, pragmaForeignKeyList,
		pragmaCollationList, pragmaCompileOptions, pragmaFunctionList, pragmaModuleList, pragmaPragmaList:
		return true
	default:
		return false
	}
}
