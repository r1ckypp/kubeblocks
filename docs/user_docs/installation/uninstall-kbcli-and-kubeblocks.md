---
title: Uninstall kbcli and KubeBlocks
description: Handle exception and uninstall kbcli and KubeBlocks
keywords: [kbcli, kubeblocks, exception, uninstall]
sidebar_position: 5
sidebar_label: Uninstall KubeBlocks and kbcli
---

import Tabs from '@theme/Tabs';
import TabItem from '@theme/TabItem';


# Uninstall KubeBlocks and kbcli

Uninstallation order:

1. Delete your cluster if you have created a cluster.

   ```bash
   kbcli cluster delete <name>
   ```

2. Uninstall KubeBlocks.

3. Uninstall kbcli.

## Uninstall KubeBlocks

Uninstall KubeBlocks if you want to delete KubeBlocks after your trial.

<Tabs>
<TabItem value="kbcli" label="kbcli" default>

```bash
kbcli kubeblocks uninstall
```

</TabItem>

<TabItem value="helm" label="helm" default>

```bash
helm uninstall kubeblocks --namespace kb-system
```

Helm does not delete CRD objects. You can delete the ones KubeBlocks created with the following commands:
```
kubectl get crd -o name | grep kubeblocks.io | xargs kubectl delete
```


</TabItem>

<TabItem value="yaml" label="yaml" default>

You can generate YAMLs from the KubeBlocks chart and uninstall using kubectl.

```bash
helm template kubeblocks kubeblocks/kubeblocks --namespace kb-system | kubectl delete -f -
```


</TabItem>

</Tabs>


## Uninstall kbcli

Uninstall kbcli if you want to delete kbcli after your trial. Use the same option as the way you install kbcli.

<Tabs>
<TabItem value="macOS" label="macOS" default>

For `curl`, run

```bash
sudo rm /usr/local/bin/kbcli
```

For `brew`, run

```bash
brew uninstall kbcli
```

kbcli creates a hidden folder named `~/.kbcli` under the HOME directory to store configuration information and temporary files. You can delete this folder after uninstalling kbcli.

</TabItem>

<TabItem value="Windows" label="Windows">

1. Go to the `kbcli` installation path and delete the installation folder.

   * If you install `kbcli` by script, go to `C:\Program Files` and delete the `kbcli-windows-amd64` folder.
   * If you customize the installation path, go to your specified path and delete the installation folder.

2. Delete the environment variable.

   1. Click the Windows icon and click **System**.
   2. Go to **Settings** -> **Related Settings** -> **Advanced system settings**.
   3. On the **Advanced** tab, click **Environment Variables**.
   4. Double-click **Path** in **User variables** or **System variables** list.
      * If you install `kbcli` by script, double-click **Path** in **User variables**.
      * If you customize the installation path, double-click **Path** based on where you created the variable before.
   5. Select `C:\Program Files\kbcli-windows-amd64` or your customized path and delete it. This operation requires double confirmation.

3. Delete a folder named `.kbcli`.

   kbcli creates a folder named `.kbcli` under the C:\Users\username directory to store configuration information and temporary files. You can delete this folder after uninstalling kbcli.

</TabItem>

<TabItem value="Linux" label="Linux">

Uninstall kbcli using the `curl` command.

```bash
sudo rm /usr/local/bin/kbcli
```

kbcli creates a hidden folder named `~/.kbcli` under the HOME directory to store configuration information and temporary files. You can delete this folder after uninstalling kbcli.

</TabItem>

</Tabs>
