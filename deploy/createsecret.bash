kubectl create namespace keda
kubectl create secret tls keda-tls-secret --namespace=keda --cert=apiserver.pem --key=apiserver-key.pem