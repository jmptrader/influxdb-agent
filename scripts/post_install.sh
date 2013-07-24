#!/usr/bin/env bash

echo "Linking new version"

ln -sfn /data/errplane-agent/versions/REPLACE_VERSION /data/errplane-agent/current
ln -sfn /data/errplane-agent/current/agent /usr/bin/errplane-agent
ln -sfn /data/errplane-agent/shared/log.txt /data/errplane-agent/current/log.txt

if [ ! -e "/etc/errplane-agent/config.yml" ]; then
    cp /data/errplane-agent/current/sample_config.yml /etc/errplane-agent/config.yml
fi

chown errplane:errplane -R /etc/errplane-agent/config.yml
chown errplane:errplane -R /data/errplane-agent/current
chown errplane:errplane -R /usr/bin/errplane-agent

update-rc.d -f errplane-agent remove
update-rc.d errplane-agent defaults

echo "Finished updating the Errplane agent"
