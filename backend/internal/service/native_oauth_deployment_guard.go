package service

// A malformed declaration must fail closed rather than become an ordinary OAuth account.
func CoreOAuthDeclared(account *Account) bool {
	if account == nil || account.Extra == nil {
		return false
	}
	_, declared := account.Extra[CoreOAuthAccountExtraKey]
	return declared
}

// Ordinary CRUD cannot mint, replace or remove core-owned identity or credentials.
// Scheduling fields remain independent and can still be changed explicitly.
func RejectCoreOAuthAdminMutation(current *Account, inputExtra, inputCredentials map[string]any) error {
	if _, declared := inputExtra[CoreOAuthAccountExtraKey]; declared {
		return ErrCoreOAuthConflict
	}
	if CoreOAuthDeclared(current) && (inputExtra != nil || inputCredentials != nil) {
		return ErrCoreOAuthConflict
	}
	return nil
}
