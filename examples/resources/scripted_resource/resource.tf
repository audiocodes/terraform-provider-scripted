# A record kept by manage.py (see below), which stores each record as a JSON
# file. Swap the script for one that talks to a real API and nothing here
# changes.
resource "scripted_resource" "record" {
  program     = ["python3", "${path.module}/manage.py"]
  environment = { STORE_DIR = "${path.module}/store" }
  timeout     = "2m"

  input = {
    name = "web-1"
    kind = "vm" # the script asks for replacement when this changes
    size = 2
  }
}

output "address" {
  value = scripted_resource.record.output.address
}

# Several of them from a map, the usual shape once there is more than one.
variable "records" {
  type = map(object({ kind = string, size = number }))
  default = {
    db-1 = { kind = "vm", size = 4 }
    db-2 = { kind = "container", size = 1 }
  }
}

resource "scripted_resource" "records" {
  for_each = var.records

  program     = ["python3", "${path.module}/manage.py"]
  environment = { STORE_DIR = "${path.module}/store" }

  input = merge(each.value, { name = each.key })
}

# Taking over a record created by hand: `terraform import` with the record id,
# or declaratively:
#
#   import {
#     to = scripted_resource.record
#     id = "web-1"
#   }
#
# manage.py's import handler returns the record's current spec as input, so
# the first plan after the import is clean when the configuration matches.
