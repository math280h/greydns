# Use a minimal base image with Go
FROM golang:1.24 as builder

# Set the working directory
WORKDIR /app

# Copy Go module files and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code
COPY . .

# Build the controller binary
RUN CGO_ENABLED=0 GOOS=linux go build -o controller cmd/main.go

# Use a lightweight base image
FROM alpine:latest

# Set the working directory in the final image
WORKDIR /root/

# Copy the compiled binary from the builder
COPY --from=builder /app/controller .

# Run the controller
CMD ["./controller"]
