#!/bin/bash

# Exit immediately if a command exists with a non-zero status
set -e

echo "Building gophel..."

go build -o gophel

echo "Moving gophel to /usr/local/bin/..."
sudo mv gophel /usr/local/bin/

echo "Setting executable permission"
sudo chmod +x /usr/local/bin/gophel

# Create systemd service file
echo "Creating systemd service..."
sudo tee /etc/systemd/system/gophel.service > /dev/null << EOL
[Unit]
Description=Gophel WebSocket Monitoring Service
After=network.target

[Service]
Type=simple
User=$USER
WorkingDirectory=/usr/local/bin
ExecStart=/usr/local/bin/gophel monitor
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOL

# Reload systemd to recognize new service
echo "Reloading systemd..."
sudo systemctl daemon-reload

# Enable and start the service
echo "Enabling and starting gophel service..."
sudo systemctl enable gophel.service
sudo systemctl restart gophel.service

echo "✅ Installation completed! You can now run 'gophel --help'."
echo "The WebSocket monitoring service is running in the background."
echo "To check service status: sudo systemctl status gophel"
echo "To view logs: sudo journalctl -u gophel -f"