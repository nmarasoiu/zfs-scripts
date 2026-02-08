#!/bin/bash
(echo -e "NAME\tspecial_small_blocks\trecordsize"; \
 paste <(zfs get special_small_blocks -t filesystem -o name,value -H) \
       <(zfs get recordsize -t filesystem -o value -H)) | column -t
