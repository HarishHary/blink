package backends

type DatabaseReader interface {
	ReadData() ([]map[string]interface{}, error)
}
