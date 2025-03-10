#!/bin/bash

# Exit immediately if a commend exists with a non-zero status
set -e

echo "Building gophel..."

go build -o gophel

echo "Moving gophel to /usr/local/bin/..."
sudo mv gophel /usr/local/bin/

echo "Setting executable permission"
sudo chmod +x /usr/local/bin/gophel

echo "✅ Installation completed! You can now run 'gophel --help'."