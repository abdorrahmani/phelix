#!/bin/bash

# Exit immediately if a command exists with a non-zero status
set -e

echo "Building phelix..."

go build -o phelix

echo "Moving phelix to /usr/local/bin/..."
sudo mv phelix /usr/local/bin/

echo "Setting executable permission"
sudo chmod +x /usr/local/bin/phelix

# Create startup script
echo "Creating startup script..."
sudo tee /usr/local/bin/phelix-startup.sh > /dev/null << EOL
#!/bin/bash
# Start the monitoring service
/usr/local/bin/phelix monitor &

# Wait for monitoring service to initialize
sleep 5

# Start all applications
/usr/local/bin/phelix start
EOL

# Make startup script executable
sudo chmod +x /usr/local/bin/phelix-startup.sh

# Create systemd service file
echo "Creating systemd service..."
sudo tee /etc/systemd/system/phelix.service > /dev/null << EOL
[Unit]
Description=Phelix gRPC Monitoring Service
After=network.target

[Service]
Type=simple
User=$USER
WorkingDirectory=/usr/local/bin
ExecStart=/usr/local/bin/phelix-startup.sh
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOL

# Reload systemd to recognize new service
echo "Reloading systemd..."
sudo systemctl daemon-reload

# Enable and start the service
echo "Enabling and starting phelix service..."
sudo systemctl enable phelix.service
sudo systemctl restart phelix.service

echo "✅ Installation completed! You can now run 'phelix --help'."
echo "The gRPC monitoring daemon is running in the background."
echo "To check service status: sudo systemctl status phelix"
echo "To view logs: sudo journalctl -u phelix -f"