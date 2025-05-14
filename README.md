# aknsc

A Kubernetes controller for managing namespace classes and their associated resources. This controller allows you to define a set of resources that should be applied to any namespace labeled with a specific class.

## Description

The Namespace Class Controller (aknsc) provides a way to manage and standardize resources across multiple namespaces in a Kubernetes cluster. By labeling namespaces with a specific class, you can automatically apply a predefined set of resources to those namespaces.

Key features:
- Define namespace classes with associated resources
- Automatic resource application to labeled namespaces
- Cleanup of resources when namespaces are removed from a class
- Status tracking for namespace class assignments
- Support for transitioning namespaces between classes

## Getting Started

### Prerequisites
- go version v1.23.0+
- docker.
- kubectl.
- kind (for local development)

### Local Development

1. **Start a local kind cluster:**

```sh
make kind-create
```

2. **Build and deploy to kind:**

```sh
make kind-deploy
```

This will:
- Build the controller image
- Load it into kind
- Deploy the CRDs
- Deploy the controller

3. **Deploy sample data to kind:**

```sh
make kind-deploy-manifests
```

4. **Verify the resources are applied:**

```sh
kubectl get namespaceclass internal-network -o yaml
```

5. **Clean up:**

```sh
make kind-cleanup
```