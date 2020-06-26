package protobuf

import "github.com/jhump/protoreflect/desc"

// DescriptorProvider is a source of protobuf descriptor information. It can be backed by a FileDescriptorSet
// proto (like a file generated by protoc).
type DescriptorProvider interface {
	// FindService returns a service descriptor for the given fully-qualified symbol name.
	FindService(fullyQualifiedName string) (*desc.ServiceDescriptor, error)

	Close()
}