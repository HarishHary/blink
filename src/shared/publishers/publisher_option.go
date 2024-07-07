package publishers

type PublisherOption func(*Publisher)

func Name(Name string) PublisherOption {
	return func(publisher *Publisher) {
		publisher.Name = Name
	}
}

func PublisherID(PublisherID string) PublisherOption {
	return func(publisher *Publisher) {
		publisher.PublisherID = PublisherID
	}
}

func Description(Description string) PublisherOption {
	return func(publisher *Publisher) {
		publisher.Description = Description
	}
}

func Disabled(Disabled bool) PublisherOption {
	return func(publisher *Publisher) {
		publisher.Disabled = Disabled
	}
}
