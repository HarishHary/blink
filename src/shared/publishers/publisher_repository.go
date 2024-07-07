package publishers

type PublisherRepository struct {
	Publishers map[string]*IPublisher
	isImported bool
}

func NewPublisherRepository() *PublisherRepository {
	return &PublisherRepository{
		Publishers: make(map[string]*IPublisher),
		isImported: false,
	}
}

var PublisherRepositoryInstance = NewPublisherRepository()

func (apr *PublisherRepository) ImportPublishers() {
	if !apr.isImported {
		// Assuming loadConfig and importFolders functions are implemented elsewhere
		// config := loadConfig()
		// importFolders(config.Global.General.PublisherLocations...)
		apr.isImported = true
	}
}

func (apr *PublisherRepository) GetPublisher(name string) (*IPublisher, error) {
	if apr.HasPublisher(name) {
		return apr.Publishers[name], nil
	}
	return nil, &PublisherError{Message: "Publisher not found"}
}

func (apr *PublisherRepository) HasPublisher(name string) bool {
	apr.ImportPublishers()
	_, exists := apr.Publishers[name]
	return exists
}

func (apr *PublisherRepository) RegisterPublisher(publisher *IPublisher) error {
	pub := *publisher
	if _, exists := apr.Publishers[pub.Name()]; exists {
		return &PublisherError{Message: "Publisher already registered"}
	}
	apr.Publishers[pub.Name()] = publisher
	return nil
}
